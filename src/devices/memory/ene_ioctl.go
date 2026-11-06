package memory

// Package: Memory
// Author: Nikola Jurkovic
// License: GPL-3.0 or later

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// ENE DRAM controllers are reached indirectly: a 16-bit word written to command
// 0x00 selects a register, then data moves through a fixed command byte. The
// smbus package cannot express that: it has no word write and no byte write, and
// its block write caches identical payloads per (address, register), which would
// silently drop every repeated colour triple.
const (
	eneSlave             = 0x0703
	eneSmBus             = 0x0720
	eneIoctlRead  uint8  = 1
	eneIoctlWrite uint8  = 0
	eneByteData   uint32 = 2
	eneWordData   uint32 = 3
	eneBlockData  uint32 = 5
	eneBlockMax   uint32 = 32
)

type eneI2cCommand struct {
	mode    uint8
	command uint8
	length  uint32
	pointer unsafe.Pointer
}

func eneIoctl(fd, cmd, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, cmd, arg); errno != 0 {
		return errno
	}
	return nil
}

func eneSelect(f *os.File, address uint8) error {
	return eneIoctl(f.Fd(), eneSlave, uintptr(address))
}

// eneWriteWordData writes a 16-bit value. SMBus transmits the low byte first,
// so callers pass the already-swapped register.
func eneWriteWordData(f *os.File, address, register uint8, value uint16) error {
	if err := eneSelect(f, address); err != nil {
		return err
	}

	cmd := eneI2cCommand{
		mode:    eneIoctlWrite,
		command: register,
		length:  eneWordData,
		pointer: unsafe.Pointer(&value),
	}
	return eneIoctl(f.Fd(), eneSmBus, uintptr(unsafe.Pointer(&cmd)))
}

func eneWriteByteData(f *os.File, address, register, value uint8) error {
	if err := eneSelect(f, address); err != nil {
		return err
	}

	cmd := eneI2cCommand{
		mode:    eneIoctlWrite,
		command: register,
		length:  eneByteData,
		pointer: unsafe.Pointer(&value),
	}
	return eneIoctl(f.Fd(), eneSmBus, uintptr(unsafe.Pointer(&cmd)))
}

func eneWriteBlockData(f *os.File, address, register uint8, buf []byte) error {
	if len(buf) > int(eneBlockMax) {
		return errors.New("buffer is too long for this type")
	}
	if err := eneSelect(f, address); err != nil {
		return err
	}

	data := make([]byte, 1+len(buf), eneBlockMax+2)
	data[0] = byte(len(buf))
	copy(data[1:], buf)

	cmd := eneI2cCommand{
		mode:    eneIoctlWrite,
		command: register,
		length:  eneBlockData,
		pointer: unsafe.Pointer(&data[0]),
	}
	return eneIoctl(f.Fd(), eneSmBus, uintptr(unsafe.Pointer(&cmd)))
}

func eneReadByteData(f *os.File, address, register uint8) (uint8, error) {
	if err := eneSelect(f, address); err != nil {
		return 0, err
	}

	var value uint8
	cmd := eneI2cCommand{
		mode:    eneIoctlRead,
		command: register,
		length:  eneByteData,
		pointer: unsafe.Pointer(&value),
	}
	err := eneIoctl(f.Fd(), eneSmBus, uintptr(unsafe.Pointer(&cmd)))
	return value, err
}
