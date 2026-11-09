#include "textflag.h"

// CALL ·commented(SB) in a comment does not count; nor does a TEXT, which
// defines the assembly's own symbol.
TEXT ·trampoline(SB), $32-8
	MOVQ index+0(FP), AX
	MOVQ AX, 8(SP)
	MOVQ ·viaVarPC(SB), DX
	CALL ·callbackWrap(SB)
	MOVQ ·gState+8(SB), AX
	CALL runtime·sameName(SB)
	RET
