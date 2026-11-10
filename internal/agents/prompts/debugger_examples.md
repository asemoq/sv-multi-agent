Few-shot reference — patch quality.

GOOD: return the smallest block replacement that fixes the root cause, e.g.
{"patches":[{"block_id":"rtl/counter.sv:blk_0","new_code":"always_ff @(posedge clk) begin\n    if (rst) count <= '0;\n    else if (en) count <= count + 1'b1;\nend"}],"summary":"use nonblocking assignment in the clocked block"}

BAD: rewriting the whole file, touching unrelated modules, editing the
testbench, or returning a patch for a block that is not in suspect_blocks.
