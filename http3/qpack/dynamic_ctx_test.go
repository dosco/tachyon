package qpack

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDecodeFieldSectionCtx_UnblocksOnInsert is the core property of
// Phase B: a request whose Required Insert Count runs ahead of the
// table's current InsertCount blocks until encoder-stream inserts
// catch up, then decodes successfully. Without this, advertising
// BLOCKED_STREAMS>0 would spuriously 400 every real Chrome request.
func TestDecodeFieldSectionCtx_UnblocksOnInsert(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)
	// Table empty — the section below needs one insert.

	// Build a section referencing dynamic absolute index 0 via
	// Base=1/RIC=1 (encoded RIC=2). Pattern 1000xxxx (6-bit idx=0,
	// dynamic).
	var block []byte
	block = appendQPACKInt(block, 0x00, 8, 2) // eRIC=2 → RIC=1
	block = appendQPACKInt(block, 0x00, 7, 0) // S=0 DeltaBase=0 → Base=1
	block = appendQPACKInt(block, 0x80, 6, 0) // Indexed dynamic relIdx=0

	// Kick off the decode in a goroutine; it must block.
	type result struct {
		fields      []Field
		usedDynamic bool
		err         error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		fields, _, used, err := d.DecodeFieldSectionCtx(ctx, 7, block)
		done <- result{fields, used, err}
	}()

	// The decoder should NOT have completed yet — the insert hasn't
	// arrived. Give the goroutine a scheduling slice.
	select {
	case r := <-done:
		t.Fatalf("decode returned before insert: %+v", r)
	case <-time.After(20 * time.Millisecond):
		// Expected: still blocked.
	}

	// Feed the insert. Anything with T=0 (literal name) is simplest.
	var enc []byte
	enc = appendQPACKInt(enc, 0x40, 5, 8) // Insert With Literal Name, H=0, len=8
	enc = append(enc, []byte("x-custom")...)
	enc = appendStringLiteral(enc, "v1")
	if _, err := d.HandleEncoderStream(enc); err != nil {
		t.Fatalf("HandleEncoderStream: %v", err)
	}

	// Decoder should now wake and return the decoded field.
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("decode err: %v", r.err)
		}
		if !r.usedDynamic {
			t.Errorf("usedDynamic=false for dynamic-indexed section")
		}
		if len(r.fields) != 1 || r.fields[0].Name != "x-custom" || r.fields[0].Value != "v1" {
			t.Errorf("fields=%+v", r.fields)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("decode did not unblock after insert")
	}
}

// TestDecodeFieldSectionCtx_TimeoutReturnsBlocked pins the other half
// of the contract: if the insert never arrives, the wait is bounded
// and surfaces ErrBlocked rather than leaking a goroutine forever.
func TestDecodeFieldSectionCtx_TimeoutReturnsBlocked(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)

	var block []byte
	block = appendQPACKInt(block, 0x00, 8, 2) // RIC=1, table empty
	block = appendQPACKInt(block, 0x00, 7, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := d.DecodeFieldSectionCtx(ctx, 1, block)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v want ErrBlocked", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("returned too fast (%v) — wait path not exercised", elapsed)
	}
}

// TestDecodeFieldSection_StaticOnly_NotUsedDynamic verifies the
// gating bit that prevents spurious Section Acknowledgments on
// all-static sections — a correctness bug in the pre-Phase-B code
// that would have corrupted the peer's Known Received Count once
// dynamic inserts started flowing.
func TestDecodeFieldSection_StaticOnly_NotUsedDynamic(t *testing.T) {
	d := NewDecoder(4096)
	// Static-only section: eRIC=0, Base=0, one indexed static entry.
	var block []byte
	block = appendQPACKInt(block, 0x00, 8, 0) // eRIC=0
	block = appendQPACKInt(block, 0x00, 7, 0)
	// Static GET (index 17).
	block = appendQPACKInt(block, 0xC0, 6, 17)

	_, _, used, err := d.DecodeFieldSection(1, block)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if used {
		t.Error("usedDynamic=true for static-only section")
	}
}

