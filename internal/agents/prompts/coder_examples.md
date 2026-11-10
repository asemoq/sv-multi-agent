Few-shot reference — the same contract (synchronous reset, enable, wrap-around),
first a GOOD implementation and then a BAD one.

GOOD (synthesizable, explicit widths, one clocked process, nonblocking assigns):
module counter #(
    parameter int WIDTH = 8
) (
    input  logic             clk,
    input  logic             rst,
    input  logic             en,
    output logic [WIDTH-1:0] count
);
    always_ff @(posedge clk) begin
        if (rst) begin
            count <= '0;
        end else if (en) begin
            count <= count + 1'b1;
        end
    end
endmodule

BAD (do not imitate):
module counter #(
    parameter WIDTH = 8
) (
    input clk,
    input rst,
    input en,
    output reg [WIDTH:0] count
);
    always @(posedge clk) begin
        if (rst)
            count = 0;
        else if (en)
            count = count + 1;
    end
    always @(*) begin
        if (en)
            count = count + 1;
    end
endmodule

Why BAD is wrong: blocking assignments in a clocked process, an extra width bit
([WIDTH:0] instead of [WIDTH-1:0]), and two processes driving the same signal
(multiple drivers / latch behaviour).
