//go:build ignore

TEXT ·ignored(SB), $0-0
	CALL ·ignoredCallback(SB)
	RET
