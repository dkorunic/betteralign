package layoutpin

import "golang.org/x/crypto/ssh"

// The SSH wire format is field order.
type sshForward struct { // want `reorder skipped: x/crypto/ssh call at .*ssh.go:20:`
	sshAddr       string
	sshPort       uint32
	sshOriginAddr string
	sshOriginPort uint32
}

type sshReply struct { // want `reorder skipped: x/crypto/ssh call at .*ssh.go:21:`
	sshOK   bool
	sshText string
	sshCode uint32
}

func sshPayloads(f sshForward, r *sshReply, b []byte) error {
	_ = ssh.Marshal(f)
	return ssh.Unmarshal(b, r)
}
