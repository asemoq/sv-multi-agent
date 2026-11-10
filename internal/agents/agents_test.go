package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/sv/contract"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

const patchRTL = `module counter (
    input  logic clk,
    input  logic rst,
    input  logic en,
    output logic [7:0] count,
    output logic valid
);
    always_ff @(posedge clk) begin
        if (rst) begin
            count <= '0;
        end else if (en) begin
            count <= count + 1'b1;
        end
    end

    assign valid = en & ~rst;

    always_comb begin
        next_count = count + 1'b1;
    end
endmodule
`

func mockOptions() Options {
	return Options{
		Mode:                  "mock",
		MaxRevisions:          2,
		ContractOnly:          true,
		TopModule:             "tb",
		VerilatorBuildTimeout: 240,
		VerilatorRunTimeout:   60,
		TraceWindowEdges:      10,
	}
}

func TestMockArchitectCoderVerifier(t *testing.T) {
	ctx := context.Background()
	opts := mockOptions()

	architecture := Architect(ctx, nil, opts, "Create an 8-bit counter with enable and synchronous reset", "")
	if architecture.Status != "completed" {
		t.Fatalf("architect = %#v", architecture)
	}
	contractData, err := contract.Parse(map[string]any{"contract_json": architecture.ContractJSON})
	if err != nil {
		t.Fatal(err)
	}

	code := Coder(ctx, nil, opts, contractData, nil, "")
	if code.Status != "completed" || len(code.Artifacts) == 0 {
		t.Fatalf("coder = %#v", code)
	}
	if !strings.Contains(code.Artifacts[0].Content, "always_ff") {
		t.Fatalf("rtl = %q", code.Artifacts[0].Content)
	}

	testbench := Verifier(ctx, nil, opts, contractData, nil, "")
	if testbench.Status != "completed" || len(testbench.Artifacts) == 0 {
		t.Fatalf("verifier = %#v", testbench)
	}
	if !strings.Contains(testbench.Artifacts[0].Content, "SIMULATION PASSED") {
		t.Fatalf("tb = %q", testbench.Artifacts[0].Content)
	}
}

func TestMockArchitectRejectsUnimplemented(t *testing.T) {
	result := Architect(context.Background(), nil, mockOptions(), "Create an AXI4 Lite register bank", "")
	if result.Status != "failed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestDebuggerMockFails(t *testing.T) {
	result := Debugger(context.Background(), nil, mockOptions(), map[string]any{}, nil, nil, "")
	if result.Status != "failed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestApplyBlockPatches(t *testing.T) {
	block := slicer.ParseBlocks(patchRTL, "rtl/counter.sv")[0]
	replacement := "    always_ff @(posedge clk) begin\n" +
		"        if (rst) begin\n" +
		"            count <= '0;\n" +
		"        end else if (en) begin\n" +
		"            count <= count + 9'd1;\n" +
		"        end\n" +
		"    end"

	artifacts := ApplyBlockPatches(
		map[string]string{"rtl/counter.sv": patchRTL},
		[]any{map[string]any{"block_id": block.ID, "new_code": replacement}},
	)
	if len(artifacts) != 1 || artifacts[0].Path != "rtl/counter.sv" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	content := artifacts[0].Content
	if !strings.Contains(content, "count + 9'd1") {
		t.Fatalf("content = %q", content)
	}
	if !strings.Contains(content, "assign valid = en & ~rst;") {
		t.Fatalf("content = %q", content)
	}
	if !strings.Contains(content, "always_comb begin") {
		t.Fatalf("content = %q", content)
	}
}

func TestUnknownBlockIDIsIgnored(t *testing.T) {
	artifacts := ApplyBlockPatches(
		map[string]string{"rtl/counter.sv": patchRTL},
		[]any{map[string]any{"block_id": "rtl/counter.sv:blk_99", "new_code": "x"}},
	)
	if len(artifacts) != 0 {
		t.Fatalf("artifacts = %#v", artifacts)
	}
}

func TestMalformedPatchesAreIgnored(t *testing.T) {
	artifacts := ApplyBlockPatches(
		map[string]string{"rtl/counter.sv": patchRTL},
		[]any{nil, map[string]any{}, map[string]any{"block_id": ""}},
	)
	if len(artifacts) != 0 {
		t.Fatalf("artifacts = %#v", artifacts)
	}
}

func TestFilesResultRejectsInvalidPaths(t *testing.T) {
	invalid := filesResult(map[string]any{
		"files": []any{map[string]any{"path": "../secret.sv", "content": "x"}},
	}, "rtl")
	if invalid.Status != "failed" {
		t.Fatalf("invalid = %#v", invalid)
	}

	valid := filesResult(map[string]any{
		"files": []any{map[string]any{"path": "counter.sv", "content": "module counter; endmodule"}},
	}, "rtl")
	if valid.Status != "completed" || len(valid.Artifacts) != 1 || valid.Artifacts[0].Path != "rtl/counter.sv" {
		t.Fatalf("valid = %#v", valid)
	}
}

func TestFilesResultAcceptsDirectoryMap(t *testing.T) {
	result := filesResult(map[string]any{
		"files": map[string]any{"counter.sv": "module counter; endmodule"},
	}, "rtl")
	if result.Status != "completed" || len(result.Artifacts) != 1 || result.Artifacts[0].Path != "rtl/counter.sv" {
		t.Fatalf("result = %#v", result)
	}
}
