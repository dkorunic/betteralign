//go:build betteralign_never_set

#include "textflag.h"

TEXT ·elsewhere(SB), $0-0
	CALL ·neverCallback(SB)
	CALL ·taggedCallback<ABIInternal>(SB)
	RET
