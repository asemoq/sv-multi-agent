package slicer

import (
	"testing"
)

const rtl = `module counter (
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestBlocksAreParsedWithDrivers(t *testing.T) {
	blocks := ParseBlocks(rtl, "rtl/counter.sv")
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if blocks[0].Kind != "always_ff" || blocks[0].Clocking != "posedge clk" {
		t.Fatalf("block0 = %#v", blocks[0])
	}
	if !contains(blocks[0].Writes, "count") || !contains(blocks[0].Reads, "en") {
		t.Fatalf("block0 writes/reads = %#v / %#v", blocks[0].Writes, blocks[0].Reads)
	}
	if blocks[1].Kind != "assign" || !contains(blocks[1].Writes, "valid") {
		t.Fatalf("block1 = %#v", blocks[1])
	}
	if blocks[2].Kind != "always_comb" || blocks[2].Clocking != "combinational" {
		t.Fatalf("block2 = %#v", blocks[2])
	}

	drivers := BuildDriverMap(blocks)
	if len(drivers["count"]) != 1 || drivers["count"][0] != "rtl/counter.sv:blk_0" {
		t.Fatalf("drivers[count] = %#v", drivers["count"])
	}
	if len(drivers["valid"]) != 1 || drivers["valid"][0] != "rtl/counter.sv:blk_1" {
		t.Fatalf("drivers[valid] = %#v", drivers["valid"])
	}
}

func TestBackwardSliceStopsAtFailingOutputDriver(t *testing.T) {
	blocks := ParseBlocks(rtl, "rtl/counter.sv")
	sliced, signals := DynamicSlice(blocks, []string{"count"}, 3)
	if len(sliced) != 1 || sliced[0].ID != "rtl/counter.sv:blk_0" {
		t.Fatalf("sliced = %#v", sliced)
	}
	if !contains(signals, "count") {
		t.Fatalf("signals = %#v", signals)
	}
}

func TestUnparseableRTLFallsBackToOneBlock(t *testing.T) {
	blocks := ParseBlocks("this is not verilog\n", "rtl/bad.sv")
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	if blocks[0].Kind != "module_body" || blocks[0].ID != "rtl/bad.sv:blk_0" {
		t.Fatalf("block = %#v", blocks[0])
	}
}

func TestParseSourcesPrefixesIDsWithFile(t *testing.T) {
	blocks := ParseSources(map[string]string{"rtl/b.sv": rtl, "rtl/a.sv": rtl})
	if blocks[0].ID != "rtl/a.sv:blk_0" {
		t.Fatalf("blocks[0].ID = %q", blocks[0].ID)
	}
	if blocks[3].ID != "rtl/b.sv:blk_0" {
		t.Fatalf("blocks[3].ID = %q", blocks[3].ID)
	}
}
