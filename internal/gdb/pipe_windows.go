package gdb

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// debuggerPipe opens both ends locally, before either is exposed to the
// child. The parent end is overlapped and has an independent deadline owner.
// GDB receives a synchronous peer as an ordinary standard stream.
func debuggerPipe(parentReads bool) (parentPipe, *os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve GDB pipe owner: %w", err)
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, nil, fmt.Errorf("protect GDB pipe: %w", err)
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}

	var nonce [16]byte
	rand.Read(nonce[:])
	name, err := windows.UTF16PtrFromString("\\\\.\\pipe\\mcphub-gdb-mi-" + hex.EncodeToString(nonce[:]))
	if err != nil {
		return nil, nil, fmt.Errorf("encode GDB pipe name: %w", err)
	}
	parentAccess, childAccess := uint32(windows.PIPE_ACCESS_OUTBOUND), uint32(windows.GENERIC_READ)
	if parentReads {
		parentAccess, childAccess = windows.PIPE_ACCESS_INBOUND, windows.GENERIC_WRITE
	}

	parent, err := windows.CreateNamedPipe(name,
		parentAccess|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 0, 0, 0, &attributes)
	if err != nil {
		return nil, nil, fmt.Errorf("create private GDB pipe: %w", err)
	}
	// Opening the client connects this one instance. No accept loop or
	// inheritable listener is used, and the client grants identification only.
	child, err := windows.CreateFile(name, childAccess, 0, nil, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("open private GDB pipe peer: %w", err), windows.CloseHandle(parent))
	}
	if err := qualifyDebuggerPipe(parent, child); err != nil {
		return nil, nil, errors.Join(err, windows.CloseHandle(child), windows.CloseHandle(parent))
	}

	if parentReads {
		return os.NewFile(uintptr(parent), "gdb-parent-pipe"),
			os.NewFile(uintptr(child), "gdb-child-pipe"), nil
	}
	// The write-only handle cannot query the completion modes required by
	// newer os.NewFile. Reuse the vendored native deadline owner without
	// broadening this handle's access or changing the child's synchronous peer.
	writer, err := winio.NewOpenFile(parent)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("own GDB write pipe: %w", err),
			windows.CloseHandle(parent), windows.CloseHandle(child))
	}
	file, ok := writer.(parentPipe)
	if !ok {
		return nil, nil, errors.Join(errors.New("GDB write pipe lacks deadline methods"),
			writer.Close(), windows.CloseHandle(child))
	}
	return file, os.NewFile(uintptr(child), "gdb-child-pipe"), nil
}

func qualifyDebuggerPipe(parent, child windows.Handle) error {
	var clientPID, serverPID uint32
	if err := windows.GetNamedPipeClientProcessId(parent, &clientPID); err != nil {
		return fmt.Errorf("verify GDB pipe client: %w", err)
	}
	if err := windows.GetNamedPipeServerProcessId(child, &serverPID); err != nil {
		return fmt.Errorf("verify GDB pipe server: %w", err)
	}
	if pid := windows.GetCurrentProcessId(); clientPID != pid || serverPID != pid {
		return errors.New("private GDB pipe endpoints did not connect to their creator")
	}
	return nil
}
