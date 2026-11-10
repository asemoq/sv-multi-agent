package rag

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/agents"
)

func TestChunkText(t *testing.T) {
	chunks := ChunkText("one\n\ntwo\n\nthree", 10)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	for _, chunk := range chunks {
		if len(chunk) > 10 {
			t.Fatalf("chunk too long: %q", chunk)
		}
	}
}

func TestChunkTextSplitsLongParagraph(t *testing.T) {
	chunks := ChunkText(strings.Repeat("x", 25), 10)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if len(chunks[0]) != 10 || len(chunks[1]) != 10 || len(chunks[2]) != 5 {
		t.Fatalf("lengths = %d,%d,%d", len(chunks[0]), len(chunks[1]), len(chunks[2]))
	}
}

func TestChunkRTLByModule(t *testing.T) {
	source := `module counter(
    input logic clk,
    output logic [7:0] count
);
    always_ff @(posedge clk) count <= count + 1'b1;
endmodule

module other(
    input logic clk
);
endmodule
`
	chunks := ChunkRTL("rtl/counter.sv", source)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0].Metadata["module_name"] != "counter" {
		t.Fatalf("metadata = %#v", chunks[0].Metadata)
	}
	if chunks[1].Metadata["module_name"] != "other" {
		t.Fatalf("metadata = %#v", chunks[1].Metadata)
	}
	if !strings.Contains(chunks[0].Content, "always_ff") {
		t.Fatalf("content = %q", chunks[0].Content)
	}
}

func TestChunkRTLFallback(t *testing.T) {
	chunks := ChunkRTL("rtl/bad.sv", "this is not verilog\n")
	if len(chunks) != 1 || chunks[0].Title != "rtl/bad.sv" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestChunkRTLSplitsLargeModule(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("module big(\n    input logic clk,\n    output logic [7:0] q\n);\n")
	for index := 0; index < 120; index++ {
		fmt.Fprintf(&builder, "    always_ff @(posedge clk) begin\n        q[%d] <= ~q[%d];\n    end\n", index%8, index%8)
	}
	builder.WriteString("endmodule\n")

	chunks := ChunkRTL("rtl/big.sv", builder.String())
	if len(chunks) < 10 {
		t.Fatalf("chunks = %d, want a structural split", len(chunks))
	}
	headerFound := false
	blockFound := false
	for _, chunk := range chunks {
		if strings.HasSuffix(chunk.Title, ":header") {
			headerFound = true
			if !strings.Contains(chunk.Content, "module big") {
				t.Fatalf("header chunk = %q", chunk.Content)
			}
		}
		if chunk.Metadata["block_id"] != nil {
			blockFound = true
		}
	}
	if !headerFound || !blockFound {
		t.Fatalf("header=%v block=%v chunks=%#v", headerFound, blockFound, chunks)
	}
}

func TestChunkTraceReport(t *testing.T) {
	report := map[string]any{
		"mismatch_count": 2,
		"samples":        4,
		"fail_time":      25,
		"fail_outputs":   []any{map[string]any{"signal": "count"}},
	}
	content := ChunkTraceReport(report)
	if content == "" || !strings.Contains(content, "fail_outputs") {
		t.Fatalf("content = %q", content)
	}
}

func TestChunkFix(t *testing.T) {
	content := ChunkFix("fixed reset priority", []agents.Artifact{
		{Path: "rtl/counter.sv", Content: "module counter; endmodule"},
	})
	if !strings.Contains(content, "fixed reset priority") || !strings.Contains(content, "rtl/counter.sv") {
		t.Fatalf("content = %q", content)
	}
}
