package slicer

import (
	"fmt"
	"strings"
	"testing"
)

const structuredRTL = `module counter #(
    parameter int WIDTH = 8,
    parameter int DEPTH = 4
) (
    input  logic             clk,
    input  logic             rst,
    input  logic             en,
    output logic [WIDTH-1:0] count,
    output logic             valid
);
    logic [WIDTH-1:0] next_count;
    logic [WIDTH-1:0] pipeline [0:DEPTH-1];

    always_ff @(posedge clk) begin
        if (rst) begin
            count <= '0;
        end else if (en) begin
            count <= count + 1'b1;
        end
    end

    assign valid = en & ~rst;

    function automatic logic [WIDTH-1:0] add_one(input logic [WIDTH-1:0] value);
        return value + 1'b1;
    endfunction

    task automatic clear();
        count = '0;
    endtask
endmodule
`

func TestParseStructureHeaderAndPorts(t *testing.T) {
	units := ParseStructure(structuredRTL, "rtl/counter.sv")
	if len(units) != 1 {
		t.Fatalf("units = %d", len(units))
	}
	unit := units[0]
	if unit.Kind != "module" || unit.Name != "counter" {
		t.Fatalf("unit = %#v", unit)
	}
	if len(unit.Params) != 2 {
		t.Fatalf("params = %#v", unit.Params)
	}
	want := map[string]string{
		"clk": "input", "rst": "input", "en": "input",
		"count": "output", "valid": "output",
	}
	if len(unit.Ports) != len(want) {
		t.Fatalf("ports = %#v", unit.Ports)
	}
	for _, port := range unit.Ports {
		if want[port.Name] != port.Dir {
			t.Fatalf("port %s dir = %q, want %q", port.Name, port.Dir, want[port.Name])
		}
	}
	if ports := unit.Ports[3]; ports.Name != "count" || !strings.Contains(ports.Type, "WIDTH") {
		t.Fatalf("count port = %#v", ports)
	}
}

func TestParseStructureSegments(t *testing.T) {
	units := ParseStructure(structuredRTL, "rtl/counter.sv")
	unit := units[0]
	kinds := map[string]int{}
	for _, segment := range unit.Segments {
		kinds[segment.Kind]++
	}
	if kinds["module_header"] != 1 || kinds["declarations"] != 1 {
		t.Fatalf("segments = %#v", unit.Segments)
	}
	if kinds["always_ff"] != 1 || kinds["assign"] != 1 {
		t.Fatalf("process segments = %#v", unit.Segments)
	}
	if kinds["function"] != 1 || kinds["task"] != 1 {
		t.Fatalf("function/task segments = %#v", unit.Segments)
	}
	if len(unit.Blocks) != 2 {
		t.Fatalf("blocks = %#v", unit.Blocks)
	}
}

func TestParseStructureNonANSI(t *testing.T) {
	source := `module legacy(clk, rst, q);
    input clk;
    input rst;
    output [7:0] q;
    assign q = 8'h00;
endmodule
`
	units := ParseStructure(source, "rtl/legacy.sv")
	if len(units) != 1 {
		t.Fatalf("units = %d", len(units))
	}
	dirs := map[string]string{}
	for _, port := range units[0].Ports {
		dirs[port.Name] = port.Dir
	}
	if dirs["clk"] != "input" || dirs["rst"] != "input" || dirs["q"] != "output" {
		t.Fatalf("ports = %#v", units[0].Ports)
	}
}

func TestParseStructureLargeModule(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("module big(\n    input logic clk,\n    output logic [7:0] q\n);\n")
	for index := 0; index < 500; index++ {
		fmt.Fprintf(&builder, "    always_ff @(posedge clk) begin\n        q[%d] <= ~q[%d];\n    end\n", index%8, index%8)
	}
	builder.WriteString("endmodule\n")

	units := ParseStructure(builder.String(), "rtl/big.sv")
	if len(units) != 1 {
		t.Fatalf("units = %d", len(units))
	}
	if len(units[0].Blocks) != 500 {
		t.Fatalf("blocks = %d", len(units[0].Blocks))
	}
	index := StructureIndex(units)
	if !strings.Contains(index, "rtl/big.sv:blk_0") || !strings.Contains(index, "rtl/big.sv:blk_499") {
		t.Fatalf("index does not cover all blocks")
	}
}

func TestHeaderOfIsCollapsed(t *testing.T) {
	units := ParseStructure(structuredRTL, "rtl/counter.sv")
	header := HeaderOf(units[0])
	if strings.Contains(header, "\n") {
		t.Fatalf("header contains newline: %q", header)
	}
	if !strings.HasPrefix(header, "module counter") || !strings.Contains(header, "output logic [WIDTH-1:0] count") {
		t.Fatalf("header = %q", header)
	}
}

func TestParseSourcesStructureSorted(t *testing.T) {
	units := ParseSourcesStructure(map[string]string{
		"rtl/b.sv": "module b(input logic clk); endmodule\n",
		"rtl/a.sv": "module a(input logic clk); endmodule\n",
	})
	if len(units) != 2 || units[0].Name != "a" || units[1].Name != "b" {
		t.Fatalf("units = %#v", units)
	}
}
