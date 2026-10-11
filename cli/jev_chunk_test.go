package cli

// Tests for the auto-chunking path: oversized items must split into pieces
// that each fit the marshaled budget, with aggregate semantics preserved.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChunkStringParagrahPreferred(t *testing.T) {
	d := &jevDecider{maxItemBytes: 200}
	paras := strings.Repeat("para one here\n\n", 3) + strings.Repeat("x", 500)
	chunks, chunked, err := d.chunkStates(paras)
	if err != nil || !chunked || len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d chunked=%v err=%v", len(chunks), chunked, err)
	}
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		if len(b) > 200 {
			t.Fatalf("chunk exceeds budget: %d bytes", len(b))
		}
	}
	// no content lost: reassembly contains all paragraph text
	joined := ""
	for _, c := range chunks {
		joined += c.(string)
	}
	if !strings.Contains(joined, "para one here") || !strings.Contains(joined, strings.Repeat("x", 100)) {
		t.Fatal("chunking lost content")
	}
}

func TestChunkStringNoSplitMidRune(t *testing.T) {
	d := &jevDecider{maxItemBytes: 60}
	s := strings.Repeat("æå✓日本語", 30) // multi-byte runes
	chunks, _, err := d.chunkStates(s)
	if err != nil {
		t.Fatal(err)
	}
	reassembled := ""
	for _, c := range chunks {
		reassembled += c.(string)
	}
	if reassembled != s {
		t.Fatalf("rune boundary split: lost or duplicated content")
	}
}

func TestBatchElements(t *testing.T) {
	d := &jevDecider{maxItemBytes: 300}
	elems := []any{}
	for i := 0; i < 20; i++ {
		elems = append(elems, map[string]any{"id": i, "text": strings.Repeat("y", 50)})
	}
	chunks, chunked, err := d.chunkStates(elems)
	if err != nil || !chunked || len(chunks) < 2 {
		t.Fatalf("expected batched chunks, got %d chunked=%v err=%v", len(chunks), chunked, err)
	}
	total := 0
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		if len(b) > 300 {
			t.Fatalf("batch exceeds budget: %d bytes", len(b))
		}
		arr, ok := c.([]any)
		if !ok {
			t.Fatal("array chunk is not an array")
		}
		total += len(arr)
	}
	if total != 20 {
		t.Fatalf("lost elements: %d != 20", total)
	}
}

func TestBatchObject(t *testing.T) {
	d := &jevDecider{maxItemBytes: 250}
	obj := map[string]any{}
	for _, k := range []string{"alpha", "beta", "gamma", "delta"} {
		obj[k] = strings.Repeat(k+"-", 40)
	}
	chunks, chunked, err := d.chunkStates(obj)
	if err != nil || !chunked || len(chunks) < 2 {
		t.Fatalf("expected object chunks, got %d chunked=%v err=%v", len(chunks), chunked, err)
	}
	seen := map[string]bool{}
	for _, c := range chunks {
		m, ok := c.(map[string]any)
		if !ok {
			t.Fatal("object chunk is not an object")
		}
		for k := range m {
			seen[k] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("lost keys: %v", seen)
	}
}

func TestSmallItemNotChunked(t *testing.T) {
	d := &jevDecider{maxItemBytes: 1024}
	_, chunked, err := d.chunkStates(map[string]any{"a": "b"})
	if err != nil || chunked {
		t.Fatalf("small item should not chunk: chunked=%v err=%v", chunked, err)
	}
}

func TestUnlimitedNoChunk(t *testing.T) {
	d := &jevDecider{maxItemBytes: 0}
	_, chunked, err := d.chunkStates(strings.Repeat("z", 10_000_000))
	if err != nil || chunked {
		t.Fatalf("maxItemBytes=0 must disable chunking: chunked=%v err=%v", chunked, err)
	}
}

func TestChunkStringEscapeHeavyFitsBudget(t *testing.T) {
	// BUG regression (grill run -04): escape expansion (control chars 6x,
	// quotes 2x) previously produced pieces marshaling far over budget.
	d := &jevDecider{maxItemBytes: 1000}
	// control chars expand 6x under JSON escaping
	s := strings.Repeat("ab\x01\x02\x03\n\n", 40) + strings.Repeat("tail \"quotes\" ", 60)
	chunks, chunked, err := d.chunkStates(s)
	if err != nil || !chunked {
		t.Fatalf("expected chunking, chunked=%v err=%v", chunked, err)
	}
	reassembled := ""
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		if len(b) > 1000 {
			t.Fatalf("chunk marshals to %d bytes, budget 1000 (escape expansion not verified)", len(b))
		}
		reassembled += c.(string)
	}
	if !strings.Contains(reassembled, "tail \"quotes\"") {
		t.Fatal("content lost while shrinking")
	}
}
