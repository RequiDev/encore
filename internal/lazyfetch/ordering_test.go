package lazyfetch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestCloseSetsClosingBeforeItCancels pins a statement order that no
// outcome-based test can observe.
//
// Three orderings of Close's three operations are worth distinguishing:
//
//	closing = true; cancel(); wg.Wait()   — correct, and the behavioural tests
//	                                        can detect a departure from it
//	cancel(); wg.Wait(); closing = true   — a defect: for as long as Close is
//	                                        parked in Wait, track still says
//	                                        yes, so a request already inside
//	                                        Resolve can raise the WaitGroup
//	                                        counter against a registered waiter.
//	                                        TestCloseRefusesNewFetchesBeforeItWaits
//	                                        catches this one.
//	cancel(); closing = true; wg.Wait()   — NOT a defect. The invariant Close
//	                                        needs (closing set before Wait)
//	                                        still holds. But it destroys the
//	                                        detector: that test reads
//	                                        base.Done() as the signal that Close
//	                                        has begun, which is sound only while
//	                                        the assignment comes first. Under
//	                                        this variant base.Done() fires with
//	                                        closing still unset and the
//	                                        assertion races rather than asserts.
//	                                        Measured at 30 passes out of 30 —
//	                                        see albumtracks_test.go:1022.
//
// So the third variant is invisible to every behavioural test in this package
// and leaves the second variant, which is a real defect, undetectable from then
// on. The only way to catch it is to assert the order in the source, which is
// what this does. It is not a style check: it is the guard on the guard.
func TestCloseSetsClosingBeforeItCancels(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lazyfetch.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lazyfetch.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Close" && fn.Recv != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("no Close method found in lazyfetch.go; this test cannot fail and must be fixed, " +
			"not deleted")
	}

	const missing = -1
	closingAt, cancelAt, waitAt := missing, missing, missing
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			// g.closing = true
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "closing" && closingAt == missing {
					closingAt = fset.Position(node.Pos()).Offset
				}
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "cancel" && cancelAt == missing {
				cancelAt = fset.Position(node.Pos()).Offset
			}
			if sel.Sel.Name == "Wait" && waitAt == missing {
				waitAt = fset.Position(node.Pos()).Offset
			}
		}
		return true
	})

	for name, offset := range map[string]int{"closing = true": closingAt, "cancel()": cancelAt, "wg.Wait()": waitAt} {
		if offset == missing {
			t.Fatalf("Close no longer contains %s; this test is asserting nothing about a Close it "+
				"cannot recognise", name)
		}
	}

	if !(closingAt < cancelAt) {
		t.Errorf("Close cancels before it marks itself closing. That leaves Close correct, so every " +
			"behavioural test still passes — and it silently destroys " +
			"TestCloseRefusesNewFetchesBeforeItWaits, which reads base.Done() as the signal that " +
			"Close has begun and is sound only while the assignment comes first. From then on, " +
			"moving the assignment below wg.Wait — which IS a defect — goes undetected.")
	}
	if !(cancelAt < waitAt) {
		t.Errorf("Close waits before it cancels; a fill in flight is never told to stop, so Close " +
			"blocks until the fill's own timeout expires")
	}
	if !(closingAt < waitAt) {
		t.Errorf("Close marks itself closing only after wg.Wait returns. For as long as Close is " +
			"parked in Wait, track still says yes, so a request already inside Resolve can call " +
			"wg.Add against a registered waiter — the panic 22d0f2c/M1 fixed")
	}
}
