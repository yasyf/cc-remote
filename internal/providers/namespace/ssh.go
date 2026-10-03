package namespace

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

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

var quietNative = []string{"NS_GRPC_DEBUG=0", "NSC_GRPC_DEBUG_REQUESTS=0", "NSC_GRPC_DEBUG_RESPONSES=0"}

func (p *Provider) container(compute providers.ComputeInstance, cmd []string, stdin io.Reader) providers.Command {
	return p.containerScript(compute, providers.ShellQuote(cmd...), stdin)
}

func (p *Provider) containerScript(compute providers.ComputeInstance, script string, stdin io.Reader) providers.Command {
	return providers.Command{
		Name:  p.CLI,
		Args:  []string{"ssh", "--container_name", compute.Container, "-T", compute.InstanceID, script},
		Env:   slices.Concat(quietNative, []string{"NSC_ENDPOINT=" + compute.Endpoint}),
		Stdin: stdin,
	}
}

func (p *Provider) host(compute providers.ComputeInstance, cmd []string, stdin io.Reader) providers.Command {
	return providers.Command{
		Name:  p.CLI,
		Args:  []string{"ssh", "-T", compute.InstanceID, providers.ShellQuote(cmd...)},
		Env:   slices.Concat(quietNative, []string{"NSC_ENDPOINT=" + compute.Endpoint}),
		Stdin: stdin,
	}
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
