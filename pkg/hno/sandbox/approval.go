package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ApprovalState is where one pending approval stands.
type ApprovalState int

const (
	// ApprovalPending means a human has not answered yet.
	ApprovalPending ApprovalState = iota
	// ApprovalGranted means it was answered yes; Resume may proceed.
	ApprovalGranted
	// ApprovalRefused means it was answered no; Resume is refused.
	ApprovalRefused
	// ApprovalExpired means the hold window ran out and the request is void.
	ApprovalExpired
)

// String names the state for logs and for Awaiting output.
func (s ApprovalState) String() string {
	switch s {
	case ApprovalPending:
		return "pending"
	case ApprovalGranted:
		return "granted"
	case ApprovalRefused:
		return "refused"
	case ApprovalExpired:
		return "expired"
	}
	return "unknown"
}

// PendingApproval is one suspended request, and the record of what was promised.
//
// It carries the request itself, because resuming an approval means running *that* work —
// not whatever the caller happens to send next. It also carries the environment lineage the
// approval was asked about, because an approval granted for one box must not be spent on
// another.
type PendingApproval struct {
	ID        string
	SessionID string
	RunID     string
	// Policy names which policy demanded the approval, so the answer can be attributed.
	Policy string
	Reason string
	// Request is the suspended request, cloned so a later run cannot pick up mutations
	// the caller made after the run returned.
	Request Request
	// Lineage identifies the environment the work was meant for.
	Lineage string
	State   ApprovalState

	demandedAt time.Time
	answeredAt time.Time
	expiresAt  time.Time
	approver   string
	verdict    string
	hold       time.Duration
}

// DemandedAt is when the approval was asked for.
func (p PendingApproval) DemandedAt() time.Time { return p.demandedAt }

// AnsweredAt is when it was answered, or the zero time if it has not been.
func (p PendingApproval) AnsweredAt() time.Time { return p.answeredAt }

// ExpiresAt is the end of the hold window: after it, a later answer is inert.
func (p PendingApproval) ExpiresAt() time.Time { return p.expiresAt }

// Approver names whoever answered, empty until then.
func (p PendingApproval) Approver() string { return p.approver }

// BoundToEnvironment reports whether this approval was asked about a specific environment.
//
// It is false for a request suspended before anything was provisioned, which is the normal
// ordering: the policy pass runs first precisely so a denied request costs no boot (§6.3).
// Such an approval authorises the work, not a box, so Resume cannot promise to return to
// "the same environment" — there was none to look at. Reporting that distinction is the
// difference between a permission with known scope and a vague one.
func (p PendingApproval) BoundToEnvironment() bool { return p.Lineage != "" }

// Age reports how long the request has been outstanding, measured to the answer if there
// was one and to now if there has not.
func (p PendingApproval) Age(now time.Time) time.Duration {
	at := p.answeredAt
	if p.State == ApprovalPending {
		at = now
	}
	return at.Sub(p.demandedAt)
}

// approvalBook is the handle's register of suspended requests.
//
// It lives on the handle rather than the session because a one-shot run can be denied
// approval too, and the answer has to be findable by whoever holds the id.
type approvalBook struct {
	byID map[string]*PendingApproval
}

func newApprovalBook() *approvalBook {
	return &approvalBook{byID: map[string]*PendingApproval{}}
}

// demand registers a suspended request.
func (b *approvalBook) demand(p *PendingApproval) {
	if b == nil {
		return
	}
	if b.byID == nil {
		b.byID = map[string]*PendingApproval{}
	}
	b.byID[p.ID] = p
}

// find returns the live record, or nil.
func (b *approvalBook) find(id string) *PendingApproval {
	if b == nil || b.byID == nil {
		return nil
	}
	return b.byID[id]
}

// forget retires a spent approval.
func (b *approvalBook) forget(id string) {
	if b == nil {
		return
	}
	delete(b.byID, id)
}

// pending lists the outstanding approvals for one session, oldest-first by id, expiring
// ones marked expired on the way past.
func (b *approvalBook) pending(sessionID string, now time.Time) []PendingApproval {
	if b == nil {
		return nil
	}
	var ids []string
	index := map[string]*PendingApproval{}
	for id, p := range b.byID {
		if p.SessionID != sessionID {
			continue
		}
		if p.State == ApprovalPending && !p.expiresAt.IsZero() && now.After(p.expiresAt) {
			p.State = ApprovalExpired
		}
		if p.State != ApprovalPending {
			continue
		}
		ids = append(ids, id)
		index[id] = p
	}
	sort.Strings(ids)
	list := make([]PendingApproval, 0, len(ids))
	for _, id := range ids {
		list = append(list, *index[id])
	}
	return list
}

// expireNow marks outstanding approvals past their window and reports how many it moved.
func (b *approvalBook) expireNow(now time.Time) int {
	if b == nil {
		return 0
	}
	var n int
	for _, p := range b.byID {
		if p.State != ApprovalPending {
			continue
		}
		if !p.expiresAt.IsZero() && now.After(p.expiresAt) {
			p.State = ApprovalExpired
			n++
		}
	}
	return n
}

// holdWindow resolves which window governs one approval: the per-approval Expires the
// policy asked for, else the handle's ApprovalHold.
//
// Both fields existed in the contract before anything read them, which made "approvals
// expire after five minutes" a sentence with no referent: a caller could set either one and
// watch a suspended run stay suspended forever.
func (sb *Sandbox) holdWindow(a *Approval) time.Duration {
	if a != nil && a.Expires > 0 {
		return a.Expires
	}
	if a != nil && a.Expires < 0 {
		// Negative says "do not expire this one". It has to be sayable, or a policy that
		// wants an indefinite hold has no way to ask for one and gets the handle default.
		return 0
	}
	if h := sb.cfg.Timeouts.ApprovalHold; h > 0 {
		return h
	}
	return 0
}

// Approve answers a pending approval with yes.
//
// The answer is recorded, not executed: what runs next is decided by Resume, which is the
// only path that can check whether the environment the approval was about still exists.
func (s *Session) Approve(ctx context.Context, id, approver string) error {
	return s.answer(ctx, id, ApprovalGranted, approver, "")
}

// Deny answers a pending approval with no, so the request cannot be resumed.
func (s *Session) Deny(ctx context.Context, id, reason string) error {
	return s.answer(ctx, id, ApprovalRefused, "", reason)
}

func (s *Session) answer(ctx context.Context, id string, state ApprovalState, approver, reason string) error {
	if id == "" {
		return fmt.Errorf("sandbox: approval id is required")
	}
	sb := s.sb
	// The book is guarded by the handle lock and the emit happens after it is dropped.
	// Emitting while holding a lock is how this package deadlocks: the close path takes
	// the handle lock and then the session lock, so a hook that wants them in the other
	// order can hang either side.
	sb.mu.Lock()
	p := sb.approvals.find(id)
	if p == nil || p.SessionID != s.id {
		sb.mu.Unlock()
		return &UnknownApprovalError{ID: id, SessionID: s.id}
	}
	if p.State != ApprovalPending {
		before := p.State
		sb.mu.Unlock()
		return &ApprovalAnsweredError{ID: id, State: before}
	}
	now := time.Now()
	if sb.expired(p, now) {
		held, runID := p.hold, p.RunID
		sb.mu.Unlock()
		sb.emit(ctx, Event{Kind: EventApprovalExpired, SessionID: s.id, RunID: runID, Detail: map[string]any{"approval": id, "hold": held.String()}})
		return &ApprovalExpiredError{ID: id, SessionID: s.id, Hold: held}
	}
	p.State = state
	p.answeredAt = now
	p.approver = approver
	p.verdict = reason
	runID, kind := p.RunID, EventApprovalGranted
	if state == ApprovalRefused {
		kind = EventApprovalRefused
	}
	sb.mu.Unlock()

	sb.emit(ctx, Event{Kind: kind, SessionID: s.id, RunID: runID, Detail: map[string]any{
		"approval": id, "approver": approver, "reason": reason,
	}})
	return nil
}

// expired reports whether the hold window ran out, and marks it. It does not emit: the
// caller that wants an event is the one that can drop the lock first.
func (sb *Sandbox) expired(p *PendingApproval, now time.Time) bool {
	if p.State != ApprovalPending || p.expiresAt.IsZero() {
		return false
	}
	if now.Before(p.expiresAt) {
		return false
	}
	p.State = ApprovalExpired
	return true
}

// Awaiting lists the approvals this session has outstanding.
//
// It is readable without an Observer on purpose. A suspended run that only announces
// itself through events is invisible to the exact caller that has to unblock it, which is
// how a workflow deadlocks while every component insists it reported the hang.
func (s *Session) Awaiting() []PendingApproval {
	s.sb.mu.Lock()
	defer s.sb.mu.Unlock()
	return s.sb.approvals.pending(s.id, time.Now())
}

// Resume runs the work an approval was asked about, in the environment it was asked about.
//
// Three claims are checked, each with its own typed failure because each needs a different
// answer from the caller: the verdict must be yes; the hold window must not have lapsed;
// and the environment must still be the one the approval named.
func (s *Session) Resume(ctx context.Context, id string, opts ...RunOption) (*Report, error) {
	if id == "" {
		return nil, fmt.Errorf("sandbox: approval id is required")
	}
	sb := s.sb
	sb.mu.Lock()
	p := sb.approvals.find(id)
	if p == nil || p.SessionID != s.id {
		sb.mu.Unlock()
		return nil, &UnknownApprovalError{ID: id, SessionID: s.id}
	}
	// Expiry is checked before the verdict is honoured: an answer that arrived after the
	// hold window no longer refers to work anybody is waiting on.
	if sb.expired(p, time.Now()) {
		held := p.hold
		sb.mu.Unlock()
		sb.emit(ctx, Event{Kind: EventApprovalExpired, SessionID: s.id, RunID: p.RunID, Detail: map[string]any{"approval": id, "hold": held.String()}})
		return nil, &ApprovalExpiredError{ID: id, SessionID: s.id, Hold: held}
	}
	var (
		req     Request
		wanted  string
		hold    = p.hold
		reason  = p.Reason
		verdict = p.verdict
		kind    = p.State
	)
	switch kind {
	case ApprovalPending:
		req, wanted = p.Request, p.Lineage
		sb.mu.Unlock()
		return nil, &ApprovalPendingError{
			ApprovalID: id, Reason: reason, Requested: describeRequest(&req),
			SessionID: s.id, Hold: hold,
		}
	case ApprovalRefused:
		sb.mu.Unlock()
		return nil, &ApprovalRefusedError{ID: id, SessionID: s.id, Reason: verdict}
	case ApprovalExpired:
		sb.mu.Unlock()
		return nil, &ApprovalExpiredError{ID: id, SessionID: s.id, Hold: hold}
	}
	// Granted. Copy what the run needs, then check the box identity outside the lock.
	req, wanted = p.Request, p.Lineage
	sb.mu.Unlock()
	s.mu.Lock()
	lineage := s.lineage
	envPresent := s.env != nil
	s.mu.Unlock()

	if err := s.holdsForApproval(id, wanted, lineage, envPresent); err != nil {
		s.clearApproved(id)
		return nil, err
	}

	s.noteApproved(id)
	rep, err := s.Exec(ctx, req, opts...)
	var pe *ApprovalPendingError
	if errors.As(err, &pe) {
		if pe.ApprovalID == id {
			// The grant failed to discharge the obligation. Handing back another pending
			// wait would let a caller answer the same id forever, so the loop is
			// reported as the defect it is.
			return nil, &ApprovalNotDischargedError{ID: id, SessionID: s.id}
		}
		// A different approval was demanded on the way through: a real new answer, and the
		// caller must see the new id.
		return rep, err
	}
	if err != nil {
		return rep, err
	}
	s.sb.forgetApproval(id)
	return rep, nil
}

// holdsForApproval refuses to spend an approval on an environment it was not asked for.
//
// This runs before Exec because Exec will rebuild a lost environment from the recorded
// spec — and running human-approved work inside a box that did not exist when the answer
// was given is exactly the substitution the approval existed to prevent. Losing the box is
// therefore reported, never recovered from silently.
func (s *Session) holdsForApproval(id, wanted, lineage string, envPresent bool) error {
	if !envPresent {
		return &ApprovalEnvironmentChangedError{ID: id, SessionID: s.id, Wanted: wanted, Got: "<no environment>"}
	}
	if lineage == "" {
		return &ApprovalEnvironmentChangedError{ID: id, SessionID: s.id, Wanted: wanted, Got: "<unknown lineage>"}
	}
	if wanted != "" && wanted != lineage {
		return &ApprovalEnvironmentChangedError{ID: id, SessionID: s.id, Wanted: wanted, Got: lineage}
	}
	return nil
}

// noteApproved records that this session holds a live grant for an id, which is what lets
// the pipeline discharge the obligation instead of asking again.
func (s *Session) noteApproved(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.approved {
		if have == id {
			return
		}
	}
	s.approved = append(s.approved, id)
}

// wasApproved reports whether this session holds a live grant for id.
func (s *Session) wasApproved(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.approved {
		if have == id {
			return true
		}
	}
	return false
}

// forgetApproval retires a spent approval from the register.
func (sb *Sandbox) forgetApproval(id string) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.approvals != nil {
		sb.approvals.forget(id)
	}
}

// demandApproval records a suspension so it can be answered and resumed, and returns the
// error the run path reports.
func (sb *Sandbox) demandApproval(rc *RunContext, dec *Decision, a *Approval) *ApprovalPendingError {
	if a.ID == "" {
		a.ID = rc.RunID
	}
	now := time.Now()
	hold := sb.holdWindow(a)
	p := &PendingApproval{
		ID:         a.ID,
		SessionID:  rc.SessionID,
		RunID:      rc.RunID,
		Policy:     dec.Policy,
		Reason:     dec.Reason,
		Request:    rc.Request.clone(),
		Lineage:    rc.lineage,
		State:      ApprovalPending,
		demandedAt: now,
		approver:   a.Approver,
		hold:       hold,
	}
	if hold > 0 {
		p.expiresAt = now.Add(hold)
	}
	sb.mu.Lock()
	if sb.approvals == nil {
		sb.approvals = newApprovalBook()
	}
	sb.approvals.demand(p)
	sb.mu.Unlock()

	sb.emit(context.Background(), Event{Kind: EventApprovalRequested, SessionID: rc.SessionID, RunID: rc.RunID, Detail: map[string]any{
		"approval": p.ID, "policy": p.Policy, "reason": p.Reason, "hold": hold.String(),
	}})
	return &ApprovalPendingError{
		ApprovalID: p.ID,
		Reason:     p.Reason,
		Requested:  describeRequest(rc.Request),
		SessionID:  rc.SessionID,
		Hold:       hold,
		ExpiresAt:  p.expiresAt,
	}
}

// expireApprovals sweeps outstanding approvals past their window and reports how many it
// moved. A hung approval is not only a stalled workflow: it is a hold on an environment
// nobody is still asking about, so it is swept alongside idle sessions.
func (sb *Sandbox) expireApprovals(now time.Time) int {
	sb.mu.Lock()
	book := sb.approvals
	sb.mu.Unlock()
	if book == nil {
		return 0
	}
	return book.expireNow(now)
}

// grantedElsewhere reports whether the session running this request already holds a grant
// for the id a policy is about to demand.
//
// The check belongs on the run context because a one-shot run has no session to consult,
// and such a run can never have been approved: it suspends and is finished.
func (rc *RunContext) grantedElsewhere(id string) bool {
	if rc.session == nil || id == "" {
		return false
	}
	return rc.session.wasApproved(id)
}

// outstanding counts suspended approvals across every session on the handle.
func (b *approvalBook) outstanding() int {
	if b == nil {
		return 0
	}
	var n int
	for _, p := range b.byID {
		if p.State == ApprovalPending {
			n++
		}
	}
	return n
}

// clearApproved drops a grant that could not be spent, so a later answer is not blocked by
// a stale local record.
func (s *Session) clearApproved(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.approved {
		if have == id {
			s.approved = append(s.approved[:i], s.approved[i+1:]...)
			return
		}
	}
}
