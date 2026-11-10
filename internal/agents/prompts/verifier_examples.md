Few-shot reference — self-checking testbench conventions.

GOOD: mirror the contract in a reference model, sample the DUT after a small
delay following the active edge, count samples and mismatches, and print the
exact markers:
    always_ff @(posedge clk) begin
        if (rst) expected <= '0;
        else if (en) expected <= expected + 1'b1;
    end
    task automatic check();
        samples++;
        if (count !== expected) begin
            mismatches++;
            if (mismatches == 1) first_fail_time = $time;
        end
    endtask
    ...
    $display("Mismatches: %0d in %0d samples", mismatches, samples);
    if (mismatches == 0) begin
        $display("SIMULATION PASSED");
    end else begin
        $display("SIMULATION FAILED - %0d MISMATCHES DETECTED, FIRST AT TIME %0d", mismatches, first_fail_time);
    end

BAD (do not imitate): driving stimulus on the active edge, sampling with no
delay, never comparing against a reference, and printing "TEST DONE" instead of
the required markers.
