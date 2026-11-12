Few-shot reference — refactor quality.

GOOD: rename a module and update its single instantiation (return both files):
{"files":{"rtl/old_name.sv":"module new_name (...);\n ...\nendmodule","rtl/top.sv":"... new_name u_new (...); ..."},"summary":"rename old_name to new_name and update its instantiation"}

GOOD: extract a block into a new submodule and replace the original block with an instance of that submodule (return the new file plus the edited parent file).

GOOD: move a module to a new file and delete the old one:
{"files":{"rtl/new/pcie_engine.sv":"module pcie_engine (...); ... endmodule"},"delete":["rtl/pcie_engine.sv"],"summary":"move pcie_engine into rtl/new/"}

GOOD: a tiny fix inside one block via a patch:
{"patches":[{"block_id":"rtl/counter.sv:blk_0","new_code":"always_ff @(posedge clk) begin\n    if (rst) count <= '0;\n    else if (en) count <= count + 1'b1;\nend"}],"summary":"restore nonblocking assignment in the clocked block"}

BAD: changing behavior, adding or removing ports, reformatting unrelated files, editing the testbench when not asked, or returning a path outside rtl/ or tb/.
