package namespace

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	framePrefix  = "CCRNSX1\x00"
	frameNonce   = 32
	recordHeader = 5
	recordStdout = 1
	recordStderr = 2
	recordStatus = 3
)

const (
	sshClient             = "ssh"
	sshDefaultPort        = 22
	connectTimeoutSeconds = 10
	connectionAttempts    = 6
	sshUnreachable        = 255
)

const frameWrapper = `import os, selectors, struct, subprocess, sys
nonce = bytes.fromhex(sys.argv[1])
if sys.argv[2]:
    os.environ["LC_ALL"] = sys.argv[2][1:]
else:
    del os.environ["LC_ALL"]
def emit(data):
    view = memoryview(data)
    while view:
        view = view[os.write(1, view):]
emit(b"CCRNSX1\0" + nonce)
try:
    child = subprocess.Popen(sys.argv[3:], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
except OSError as error:
    sys.stderr.write("cc-remote: cannot start the command: %s\n" % error)
    sys.exit(127)
streams = {child.stdout.fileno(): 1, child.stderr.fileno(): 2}
selector = selectors.DefaultSelector()
for fd in streams:
    selector.register(fd, selectors.EVENT_READ)
while streams:
    for key, _ in selector.select():
        chunk = os.read(key.fd, 65536)
        if chunk:
            emit(struct.pack(">BI", streams[key.fd], len(chunk)) + chunk)
        else:
            selector.unregister(key.fd)
            del streams[key.fd]
code = child.wait()
emit(struct.pack(">BIi", 3, 4, code if code >= 0 else -1))
`

var errFraming = errors.New("the native command stream is not this call's complete framing")

func (p *Provider) ssh(ctx context.Context, compute providers.ComputeInstance, container, script string, stdin io.Reader) (result providers.Result, err error) {
	config, err := p.sshConfig(ctx, compute, container)
	if err != nil {
		return providers.Result{}, err
	}
	host, port, err := hostPort(config.GetEndpoint())
	if err != nil {
		return providers.Result{}, fmt.Errorf("the ssh endpoint %q of namespace instance %s: %w", config.GetEndpoint(), compute.InstanceID, err)
	}
	dir, err := os.MkdirTemp("", "cc-remote-namespace-ssh-")
	if err != nil {
		return providers.Result{}, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	target := providers.Target{
		Host:          host,
		Port:          port,
		User:          config.GetUsername(),
		IdentityFile:  filepath.Join(dir, "key"),
		HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyPinned, Alias: compute.InstanceID, KnownHostsFile: filepath.Join(dir, "known_hosts")},
	}
	if err = os.WriteFile(target.IdentityFile, config.GetSshPrivateKey(), 0o600); err != nil {
		return providers.Result{}, err
	}
	if err = os.WriteFile(target.HostKeyPolicy.KnownHostsFile, knownHosts(target.HostKeyPolicy.Alias, config.GetSshHostKeys()), 0o600); err != nil {
		return providers.Result{}, err
	}
	args := []string{"-F", "none", "-T"}
	for _, option := range append(target.SSHOptions(),
		"IdentityAgent=none",
		"BatchMode=yes",
		"GlobalKnownHostsFile=/dev/null",
		"LogLevel=ERROR",
		"ConnectTimeout="+strconv.Itoa(connectTimeoutSeconds),
		"ConnectionAttempts="+strconv.Itoa(connectionAttempts),
	) {
		args = append(args, "-o", option)
	}
	return p.Runner.Run(ctx, providers.Command{Name: sshClient, Args: append(args, "--", compute.InstanceID, script), Stdin: stdin})
}

func (p *Provider) sshConfig(ctx context.Context, compute providers.ComputeInstance, container string) (*computev1beta.GetSSHConfigResponse, error) {
	var config *computev1beta.GetSSHConfigResponse
	if err := p.call(ctx, compute.Endpoint, func(ctx context.Context, client computev1beta.ComputeServiceClient) (err error) {
		config, err = client.GetSSHConfig(ctx, &computev1beta.GetSSHConfigRequest{InstanceId: compute.InstanceID, TargetContainer: container})
		return err
	}); err != nil {
		return nil, fmt.Errorf("the ssh config of namespace instance %s: %w", compute.InstanceID, err)
	}
	var missing string
	switch {
	case len(config.GetSshPrivateKey()) == 0:
		missing = "private key"
	case config.GetUsername() == "":
		missing = "username"
	case config.GetEndpoint() == "":
		missing = "endpoint"
	case len(config.GetSshHostKeys()) == 0:
		missing = "host keys"
	default:
		return config, nil
	}
	return nil, fmt.Errorf("the ssh config of namespace instance %s carries no %s", compute.InstanceID, missing)
}

func hostPort(endpoint string) (string, int, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return endpoint, sshDefaultPort, nil
	}
	number, err := strconv.Atoi(port)
	return host, number, err
}

func knownHosts(alias string, keys [][]byte) []byte {
	var lines []byte
	for _, key := range keys {
		lines = fmt.Appendf(lines, "%s %s\n", alias, bytes.TrimSpace(key))
	}
	return lines
}

func framed(nonce []byte, cmd []string) string {
	wrapper := providers.ShellQuote("python3", "-I", "-S", "-c", frameWrapper, hex.EncodeToString(nonce))
	return "LC_ALL=C " + wrapper + ` "${LC_ALL+=$LC_ALL}" ` + providers.ShellQuote(cmd...)
}

func unframe(stream, nonce []byte) (providers.Result, int, error) {
	var output providers.Result
	head := append([]byte(framePrefix), nonce...)
	if len(stream) < len(head) {
		return output, 0, fmt.Errorf("%w: the stream ends inside its %d-byte prefix", errFraming, len(head))
	}
	if !bytes.Equal(stream[:len(head)], head) {
		return output, 0, fmt.Errorf("%w: the stream does not open with this call's prefix and nonce", errFraming)
	}
	rest := stream[len(head):]
	for len(rest) > 0 {
		at := len(stream) - len(rest)
		if len(rest) < recordHeader {
			return output, 0, fmt.Errorf("%w: the stream ends inside the record header at byte %d", errFraming, at)
		}
		tag, size := rest[0], binary.BigEndian.Uint32(rest[1:recordHeader])
		body := rest[recordHeader:]
		if int64(size) > int64(len(body)) {
			return output, 0, fmt.Errorf("%w: the record at byte %d declares %d bytes but %d remain", errFraming, at, size, len(body))
		}
		payload, next := body[:size], body[size:]
		switch tag {
		case recordStdout:
			output.Stdout = append(output.Stdout, payload...)
		case recordStderr:
			output.Stderr = append(output.Stderr, payload...)
		case recordStatus:
			if size != 4 {
				return output, 0, fmt.Errorf("%w: the status record at byte %d carries %d bytes, not 4", errFraming, at, size)
			}
			if len(next) != 0 {
				return output, 0, fmt.Errorf("%w: %d bytes follow the status record at byte %d", errFraming, len(next), at)
			}
			var status int32
			if err := binary.Read(bytes.NewReader(payload), binary.BigEndian, &status); err != nil {
				return output, 0, fmt.Errorf("decode the status record: %w", err)
			}
			return output, int(status), nil
		default:
			return output, 0, fmt.Errorf("%w: the record at byte %d has unknown type %d", errFraming, at, tag)
		}
		rest = next
	}
	return output, 0, fmt.Errorf("%w: the stream ends without a status record", errFraming)
}
