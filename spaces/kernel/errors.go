package kernel

import (
	"fmt"

	"github.com/gostdlib/base/errors"
)

// The errors the kernel returns. Every one is matched with errors.Is, and every error the kernel produces
// wraps exactly one of them, so a caller branches on identity rather than on message text.
//
// The cause, where there is one, is wrapped too, so errors.Is reaches it as well: a Publish refused because
// the bus was closed matches both ErrStoppedOrNotStarted and subscriber.ErrClosed.
//
// Every error the kernel returns is permanent, so errors.Is(err, errors.ErrPermanent) reports true for all
// of them and a caller running a kernel call under retry/exponential stops rather than retrying something
// that can never succeed. Conditions a caller cannot recover from at all, such as starting a kernel twice
// or registering a duplicate module name, panic instead of returning an error.
var (
	// ErrStoppedOrNotStarted is a call made against a kernel that is not running, whether because Start has
	// not been called yet or because the kernel has been stopped by Stop or by a failed Start.
	ErrStoppedOrNotStarted = errors.New("kernel has been stopped or not started")
	// ErrInvalidArg is a missing or malformed argument.
	ErrInvalidArg = errors.New("invalid argument")
	// ErrModuleFailed is a module returning an error from Init or Start. The kernel is torn down when this
	// happens, so it is terminal for the kernel and not only for the call.
	ErrModuleFailed = errors.New("module failed")
	// ErrBus is the message bus or the background task manager refusing an operation.
	ErrBus = errors.New("message bus or background task manager failed")
)

// permanent marks err as one no retry can clear, which is every error the kernel returns. It is a no-op if
// something already in the chain carries the marker, so an error wrapping a cause that is itself permanent
// reads with one "permanent error" at the end rather than two.
func permanent(err error) error {
	if errors.Is(err, errors.ErrPermanent) {
		return err
	}
	return fmt.Errorf("%w: %w", err, errors.ErrPermanent)
}
