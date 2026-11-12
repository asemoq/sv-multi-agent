Few-shot reference — bug quality.

GOOD:
{"bugs":[{"file":"rtl/counter.sv","line":8,"severity":"warning","category":"arithmetic","title":"counter wraps one count early","description":"count <= count + 1 has no modulo, so it does not wrap at 2**WIDTH before being compared.","suggested_fix":"let count overflow naturally with WIDTH bits, or add an explicit wrap check"}],"summary":"one off-by-one in the counter"}

BAD: reporting style/naming, inventing bugs that are not in the code, or returning patches/files instead of a bugs list.
