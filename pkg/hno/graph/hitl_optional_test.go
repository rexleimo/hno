package graph_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/graph"
)

// validateResponse's own comment states that an absent property is governed only by
// "required" and is skipped here. It wasn't: the loop looked the name up in the response and
// passed the zero value for a missing key, so declaring an optional property made every
// response that legitimately omitted it invalid. A caller could not describe "answer yes or
// no, and optionally tell me why" without forcing the reason to be present.

func optionalSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"approve"},
		"properties": map[string]any{
			"approve": map[string]any{"type": "boolean"},
			"note":    map[string]any{"type": "string"},
		},
	}
}

// runToInterrupt drives a graph that suspends with the given schema, and reports how many
// times the interrupting node body ran.
func runToInterrupt(t *testing.T, schema map[string]any, calls *int) *graph.Graph {
	t.Helper()
	g := graph.New().
		AddNode(graph.NodeFunc("a", func(_ context.Context, _ any) (any, error) { return "a#1", nil })).
		AddNode(graph.NodeFunc("b", func(_ context.Context, _ any) (any, error) {
			*calls++
			if *calls == 1 {
				return nil, graph.RequestInterrupt(graph.Interrupt{
					InterruptID: "i-1", Message: "approve?", ResponseSchema: schema,
				})
			}
			return "b#resumed", nil
		})).
		AddEdge("a", "b").
		SetEntry("a").SetOutput("b")
	if _, err := g.Run(context.Background(), "go"); !errors.Is(err, graph.ErrSuspended) {
		t.Fatalf("首跑未挂起: %v", err)
	}
	return g
}

// TestOptionalPropertyMayBeAbsent is the falsifier: against the previous code the response was
// rejected with `field "note" should be string, got <nil>`.
func TestOptionalPropertyMayBeAbsent(t *testing.T) {
	calls := 0
	g := runToInterrupt(t, optionalSchema(), &calls)

	res, err := g.Resume(context.Background(), map[string]any{"i-1": map[string]any{"approve": true}})
	if err != nil {
		t.Fatalf("省略可选字段的合法响应被拒绝: %v", err)
	}
	if got, want := res.Output(), "b#resumed"; got != want {
		t.Fatalf("Output = %v, want %v", got, want)
	}
	if calls != 2 {
		t.Fatalf("节点应保持可再入，执行次数 = %d, want 2", calls)
	}
}

// TestOptionalPropertyIsStillCheckedWhenPresent blocks the wrong fix. Dropping optional
// properties from validation entirely would make this response pass.
func TestOptionalPropertyIsStillCheckedWhenPresent(t *testing.T) {
	calls := 0
	g := runToInterrupt(t, optionalSchema(), &calls)

	if _, err := g.Resume(context.Background(), map[string]any{
		"i-1": map[string]any{"approve": true, "note": 7},
	}); !errors.Is(err, graph.ErrInvalidResponse) {
		t.Fatalf("可选字段出现但类型错时必须拒, got %v", err)
	}
}

// TestExplicitNullIsNotAnAbsentValue pins the distinction the fix relies on: absence is decided
// by key presence, not by a nil value. A response that says "note": null is making a
// declaration, and it is not a string.
func TestExplicitNullIsNotAnAbsentValue(t *testing.T) {
	calls := 0
	g := runToInterrupt(t, optionalSchema(), &calls)

	if _, err := g.Resume(context.Background(), map[string]any{
		"i-1": map[string]any{"approve": true, "note": nil},
	}); !errors.Is(err, graph.ErrInvalidResponse) {
		t.Fatalf("显式 null 不是「省略」, got %v", err)
	}
}

// TestRequiredStillEnforcedAlongsideOptionals keeps the required gate intact: with an optional
// field available, omitting the required one must still fail.
func TestRequiredStillEnforcedAlongsideOptionals(t *testing.T) {
	calls := 0
	g := runToInterrupt(t, optionalSchema(), &calls)

	if _, err := g.Resume(context.Background(), map[string]any{
		"i-1": map[string]any{"note": "because"},
	}); !errors.Is(err, graph.ErrInvalidResponse) {
		t.Fatalf("缺必填字段必须拒, got %v", err)
	}
}

// TestNothingRequiredAcceptsAnEmptyResponse makes the loosening explicit rather than incidental:
// a schema whose properties are all optional no longer demands any of them, which is what
// "optional" means. required is the only lever that forces an answer, and that is now a tested
// contract rather than an accident of a broken skip.
func TestNothingRequiredAcceptsAnEmptyResponse(t *testing.T) {
	calls := 0
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"note": map[string]any{"type": "string"},
		},
	}
	g := runToInterrupt(t, schema, &calls)

	if _, err := g.Resume(context.Background(), map[string]any{"i-1": map[string]any{}}); err != nil {
		t.Fatalf("全可选 schema 应接受空响应, got %v", err)
	}
}
