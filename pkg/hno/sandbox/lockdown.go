package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// --- Decision sugar --------------------------------------------------------

// Allow returns an undecided decision. A hook that wants to abstain returns
// this rather than pretending to have decided something (§7).
func Allow() Decision { return Decision{Action: ActAllow} }

// Deny returns a veto with a reason the model can read.
func Deny(reason string) Decision {
	return Decision{Action: ActDeny, Reason: reason}
}

// DenyRule returns a veto tied to the rule that fired, which is what makes an
// audit trail searchable rather than merely long.
func DenyRule(reason, rule string) Decision {
	return Decision{Action: ActDeny, Reason: reason, Rule: rule}
}

// Approve suspends the run for a human decision (§7).
func Approve(reason string) Decision {
	return Decision{Action: ActRequireApproval, Reason: reason}
}

// ApproveBy suspends the run and names who must decide, so a graph node can
// route the interrupt to the right reviewer.
func ApproveBy(reason, approver string) Decision {
	return Decision{Action: ActRequireApproval, Reason: reason, Approval: &Approval{Approver: approver, Reason: reason}}
}

// Tighten returns an approval-with-side-effects: the request is modified in
// place, and only in a stricter direction (§7).
func Tighten(mutate func(*Request)) Decision {
	return Decision{Action: ActAllow, Tighten: mutate}
}

// --- Exported resilience helpers -------------------------------------------

// NewMemoryJournal returns an in-process journal, useful for tests, single
// process deployments, and as the default when a caller promises delivery
// semantics without supplying storage (§16.3).
func NewMemoryJournal() JournalStore { return newMemoryJournal() }

// BudgetFor reports how many attempts one failure class is granted: the class
// budget when present, otherwise MaxAttempts. A user-code class always gets
// zero, because re-running identical code is not retrying, it is repeating
// (§16.3).
func (b RetryBudget) BudgetFor(class FailureClass) int {
	if class == ClassUserCode || class == ClassDenied {
		return 0
	}
	return b.attempts(class)
}

// RetryAllowed is the effect gate: no matter how a failure classifies, a run
// that touched the outside world is never silently re-attempted. It needs an
// idempotency key so the journal can answer "did the first attempt commit?"
// (§16.3).
func (r Request) RetryAllowed(class FailureClass) bool {
	if r.Effect == EffectExternal && r.Delivery != DeliveryAtMostOnce {
		return false
	}
	if r.Effect == EffectWorkspace {
		// Workspace effects are only re-run after rollback or rebuild, which is
		// the caller's decision, not an automatic one.
		return class == ClassLost || class == ClassTransient
	}
	switch class {
	case ClassUserCode, ClassDenied, ClassPermanent, ClassTimeout:
		return false
	default:
		return true
	}
}

// IsModelRecoverable reports whether the fix belongs to the model: user code
// problems are, infrastructure problems are not. The graph uses this to decide
// whether a failure should become a tool result or a retry (§16.6).
func (c FailureClass) IsModelRecoverable() bool { return c == ClassUserCode }

// ClassNames lists every failure class, for diagnostics and docs.
func ClassNames() []string {
	return []string{
		ClassUnknown.String(), ClassUserCode.String(), ClassDenied.String(),
		ClassPermanent.String(), ClassTransient.String(), ClassResource.String(),
		ClassTimeout.String(), ClassLost.String(),
	}
}

// Lockdown reports the resolved environment-mutation posture (§17.3).
type Lockdown uint8

const (
	// LockdownUnset resolves to the profile default.
	LockdownUnset Lockdown = iota
	// LockdownDetect observes drift and reports it; execution continues.
	LockdownDetect
	// LockdownEnforce refuses undeclared environment mutation.
	LockdownEnforce
)

// String names the lockdown mode.
func (l Lockdown) String() string {
	switch l {
	case LockdownDetect:
		return "detect"
	case LockdownEnforce:
		return "enforce"
	case LockdownUnset:
		return ""
	default:
		return "unknown"
	}
}

// ParseLockdown reads the documented string forms.
func ParseLockdown(s string) (Lockdown, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "detect":
		return LockdownDetect, true
	case "enforce", "strict":
		return LockdownEnforce, true
	case "":
		return LockdownUnset, false
	default:
		return LockdownUnset, false
	}
}

// WithLockdown sets the posture explicitly. Without it, the profile decides:
// dev detects, CI and production enforce (D-15).
func WithLockdown(l Lockdown) Option {
	return func(c *Config) {
		switch l {
		case LockdownEnforce:
			c.StrictLockdown = true
		case LockdownDetect:
			c.StrictLockdown = false
		default:
		}
	}
}

// LockdownMode reports the effective posture of a handle.
func (sb *Sandbox) LockdownMode() Lockdown {
	if sb.cfg.StrictLockdown {
		return LockdownEnforce
	}
	return LockdownDetect
}

// Limits reports the resolved ceilings of a handle.
func (sb *Sandbox) Limits() Limits { return sb.cfg.Limits }

// Probe checks provider availability without running anything. It is the
// explicit form of the lazy probe Open does, so a caller can pre-flight a
// deployment and print the same UnavailableError a run would have produced
// (§5, D-07).
func (sb *Sandbox) Probe(ctx context.Context) error {
	if err := sb.bind(ctx); err != nil {
		return err
	}
	if sb.provider == nil {
		return sb.unavailable(ctx)
	}
	return probeBounded(ctx, sb.provider, sb.cfg.Timeouts.Connect)
}

// unavailable rebuilds the typed error listing what was attempted (§5.3).
func (sb *Sandbox) unavailable(ctx context.Context) error {
	names, _ := sb.candidates()
	tried := make([]Attempt, 0, len(names))
	for _, n := range names {
		prov, err := DefaultRegistry.build(ProviderConfig{Name: n})
		if err != nil {
			tried = append(tried, Attempt{Provider: n, Reason: errString(err), Fix: fixOf(err)})
			continue
		}
		iso := prov.Isolation()
		perr := probeAndClose(ctx, prov, sb.cfg.Timeouts.Connect)
		tried = append(tried, Attempt{Provider: n, Isolation: iso, Formation: sb.cfg.Formation, Reason: errString(perr), Fixes: fixesOf(perr)})
	}
	un := &UnavailableError{Tried: tried, Want: sb.cfg.MinIsolation, Fixes: defaultFixes(isoForNames(tried))}
	for _, a := range tried {
		un.Fixes = append(un.Fixes, a.Fixes...)
	}
	return un
}

// isoForNames picks the strongest isolation actually seen in an attempt list.
func isoForNames(attempts []Attempt) Isolation {
	best := IsolationUnknown
	for _, a := range attempts {
		if a.Isolation.Rank() > best.Rank() {
			best = a.Isolation
		}
	}
	return best
}

// defaultFixes gives the "nothing worked" case something actionable to say.
func defaultFixes(best Isolation) []string {
	fixes := []string{"hno sandbox doctor"}
	if best.Rank() < IsolationMicroVM.Rank() {
		fixes = append(fixes,
			"install a micro-VM engine (BoxLite needs /dev/kvm on Linux or Hypervisor.framework on macOS)",
			"or set WithMinIsolation(IsolationContainer) if container isolation is acceptable here",
		)
	}
	return fixes
}

// --- Lockdown enforcement (§17.3) ------------------------------------------

// installerTokens are the command shapes that mutate the environment outside
// the declared spec. The list is deliberately conservative: it exists to point
// the model at the right API, not to be a security boundary.
var installerTokens = []string{
	"pip install", "pip3 install", "python -m pip install", "python3 -m pip install",
	"apt-get install", "apt install", "yum install", "dnf install", "apk add", "brew install",
	"npm install", "npm i ", "yarn add", "pnpm add", "bun add",
	"go get", "cargo install", "gem install", "conda install", "pipx install",
	"curl ", "wget ",
}

// undeclaredMutation reports the mutation-looking command inside a request, if
// any. It returns the matched token plus an equivalent Install(...) hint.
func undeclaredMutation(req *Request) (string, string) {
	if req == nil {
		return "", ""
	}
	// Code and argv are both inspected: an installer invoked through argv is
	// still an installer.
	hay := strings.ToLower(req.Code + "\n" + strings.Join(req.Argv, " "))
	for _, tok := range installerTokens {
		idx := strings.Index(hay, tok)
		if idx < 0 {
			continue
		}
		line := lineAt(hay, idx)
		return strings.TrimSpace(tok), installHint(line)
	}
	return "", ""
}

// lineAt returns the line containing an offset.
func lineAt(s string, off int) string {
	start := strings.LastIndexByte(s[:off], '\n') + 1
	end := len(s)
	if nl := strings.IndexByte(s[off:], '\n'); nl >= 0 {
		end = off + nl
	}
	return strings.TrimSpace(s[start:end])
}

// installHint converts a shell installer line into the sanctioned equivalent.
// It looks only at the segment that actually installs something, so
// `cd /app && pip install x` suggests Pip("x") rather than echoing the chain.
func installHint(line string) string {
	seg := installerSegment(line)
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return ""
	}
	// Normalise `python -m pip ...` / `python3 -m pip ...` to the tool itself.
	if len(fields) >= 3 && (fields[0] == "python" || fields[0] == "python3") && fields[1] == "-m" {
		fields = append([]string{fields[2]}, fields[3:]...)
	}
	tool := strings.ToLower(fields[0])
	knownInstaller := tool
	switch tool {
	case "pip", "pip3":
		knownInstaller = "pip"
	case "apt", "apt-get", "yum", "dnf", "apk", "brew":
		knownInstaller = "apt"
	case "npm", "yarn", "pnpm", "bun":
		knownInstaller = "npm"
	case "go":
		knownInstaller = "go"
	case "curl", "wget":
		return "declare the dependency in the image, or use Setup(Shell(...)) with a pinned URL and digest"
	default:
		knownInstaller = "pip"
	}
	pkgs := []string{}
	for _, f := range fields[1:] {
		f = strings.Trim(f, "\"'\"")
		if f == "" || strings.HasPrefix(f, "-") || isInstallerSubcommand(f) {
			continue
		}
		pkgs = append(pkgs, f)
	}
	if len(pkgs) == 0 {
		return ""
	}
	switch knownInstaller {
	case "apt":
		return "Install(Apt(" + quotedList(pkgs) + "))"
	case "npm":
		return "Install(Npm(" + quotedList(pkgs) + "))"
	case "go":
		return "Install(GoGet(" + quotedList(pkgs) + "))"
	default:
		return "Install(Pip(" + quotedList(pkgs) + "))"
	}
}

// installerSegment returns the part of a shell line that contains an installer,
// splitting on the operators that chain commands.
func installerSegment(line string) string {
	units := strings.FieldsFunc(line, func(r rune) bool {
		return r == ';' || r == '&' || r == '|'
	})
	for _, u := range units {
		lowered := strings.ToLower(u)
		for _, tok := range installerTokens {
			if strings.Contains(lowered, strings.TrimSpace(tok)) {
				return strings.TrimSpace(u)
			}
		}
	}
	return line
}

// quotedList renders packages as Go string literals.
func quotedList(items []string) string {
	out := ""
	for i, p := range items {
		if i > 0 {
			out += ", "
		}
		out += `"` + p + `"`
	}
	return out
}

// checkLockdown applies D-15/D-16. In enforce mode an undeclared mutation is
// refused and the refusal tells the model what to call instead; in detect mode
// the run proceeds and the drift is recorded, because a dev box where every
// pip install needed a spec edit stops being used within a week.
func (sb *Sandbox) checkLockdown(sess *Session, req *Request) error {
	if sess == nil {
		return nil
	}
	tok, hint := undeclaredMutation(req)
	if tok == "" {
		return nil
	}
	_ = hint // carried in the Failure hint below; the denial reason stays short
	item := "undeclared environment mutation: " + tok
	sess.noteDrift(item)
	sb.emit(sessionContext(sess), Event{Kind: EventDriftDetected, SessionID: sess.id, Detail: map[string]any{"token": tok, "enforce": sb.cfg.StrictLockdown}})
	if !sb.cfg.StrictLockdown {
		return nil
	}
	// The refusal is not the end of the story: it names the API that would have
	// worked, because a model that only learns "denied" will retry the same
	// shell line forever (D-16).
	inner := &LockdownError{Rule: "undeclared-mutation", Reason: item, Suggestion: hint}
	return &Failure{
		Class:      ClassDenied,
		Retryable:  false,
		NextAction: ActionUseInstallAPI,
		Hint:       hint,
		Fixes:      []string{hint},
		Cause:      inner,
	}
}

// LockdownError is the refusal raised when a payload tries to change the
// environment outside its declared spec (§17.3).
type LockdownError struct {
	Rule       string
	Reason     string
	Suggestion string
}

// Error implements error.
func (e *LockdownError) Error() string {
	if e.Suggestion != "" {
		return fmt.Sprintf("sandbox lockdown: %s; use %s", e.Reason, e.Suggestion)
	}
	return fmt.Sprintf("sandbox lockdown: %s", e.Reason)
}

// Class marks the refusal as a policy outcome, not an infrastructure fault.
func (e *LockdownError) Class() FailureClass { return ClassDenied }

// Remediation returns the sanctioned replacement command.
func (e *LockdownError) Remediation() []string {
	if e.Suggestion == "" {
		return nil
	}
	return []string{e.Suggestion}
}

// sessionContext returns a context for session-scoped events.
func sessionContext(sess *Session) context.Context {
	if sess != nil && sess.ctx != nil {
		return sess.ctx
	}
	return context.Background()
}

// deadlineAfter returns a duration helper used by detached waits.
func deadlineAfter(d time.Duration) (time.Time, bool) {
	if d <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(d), true
}

// isInstallerSubcommand recognises the verb half of an installer invocation.
func isInstallerSubcommand(word string) bool {
	switch strings.ToLower(word) {
	case "install", "i", "add", "get", "update", "upgrade", "global":
		return true
	default:
		return false
	}
}
