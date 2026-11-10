package agents

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

func largeModule(blocks int) string {
	var builder strings.Builder
	builder.WriteString("module big(\n    input logic clk,\n    output logic [7:0] q\n);\n")
	for index := 0; index < blocks; index++ {
		fmt.Fprintf(&builder, "    always_ff @(posedge clk) begin\n        q[%d] <= ~q[%d];\n    end\n", index%8, index%8)
	}
	builder.WriteString("endmodule\n")
	return builder.String()
}

func TestSourcesContextSmallBundleIsFull(t *testing.T) {
	files := map[string]string{"rtl/counter.sv": "module counter(input logic clk);\nendmodule\n"}
	got := sourcesContext(files, 24000, nil)
	if !strings.Contains(got, "module counter") {
		t.Fatalf("content = %q", got)
	}
	if strings.Contains(got, "Source structure") {
		t.Fatalf("small bundle should stay verbatim: %q", got)
	}
}

func TestSourcesContextLargeBundleUsesStructure(t *testing.T) {
	files := map[string]string{"rtl/big.sv": largeModule(400)}
	got := sourcesContext(files, 5000, nil)
	if len(got) > 6000 {
		t.Fatalf("context length = %d", len(got))
	}
	if !strings.Contains(got, "Source structure") {
		t.Fatalf("missing structure header: %q", got[:min(200, len(got))])
	}
	if !strings.Contains(got, "rtl/big.sv: module big") {
		t.Fatal("missing unit header")
	}
	if !strings.Contains(got, "blk_0 always_ff") {
		t.Fatal("missing block index entry")
	}
	if strings.Contains(got, "q[7] <= ~q[7]") {
		t.Fatal("full block bodies leaked into a large context")
	}
}

func TestSourcesContextFocusBlocks(t *testing.T) {
	focus := []slicer.Block{{
		ID:   "rtl/big.sv:blk_0",
		Kind: "always_ff",
		Code: "always_ff @(posedge clk) begin\n    q[0] <= ~q[0];\nend",
	}}
	files := map[string]string{"rtl/big.sv": largeModule(200)}
	got := sourcesContext(files, 8000, focus)
	if !strings.Contains(got, "Relevant blocks") {
		t.Fatal("missing focus section")
	}
	if !strings.Contains(got, "q[0] <= ~q[0]") {
		t.Fatal("missing focus block code")
	}
}

func TestSourcesContextTruncates(t *testing.T) {
	files := map[string]string{"rtl/big.sv": largeModule(200)}
	got := sourcesContext(files, 200, nil)
	if !strings.Contains(got, "[truncated to fit the context budget]") {
		t.Fatalf("context was not truncated: %q", got)
	}
}
