// Copyright (C) 2014 Andreas Klauer <Andreas.Klauer@metamorpher.de>
// License: MIT

// Package nbd uses the Linux NBD layer to emulate a block device in user space
package nbd

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// @TODO: include that files directly?
const (
	// Defined in <linux/fs.h>:
	BLKROSET = 4701
	// Defined in <linux/nbd.h>:
	NBD_SET_SOCK        = 43776
	NBD_SET_BLKSIZE     = 43777
	NBD_SET_SIZE        = 43778
	NBD_DO_IT           = 43779
	NBD_CLEAR_SOCK      = 43780
	NBD_CLEAR_QUE       = 43781
	NBD_PRINT_DEBUG     = 43782
	NBD_SET_SIZE_BLOCKS = 43783
	NBD_DISCONNECT      = 43784
	NBD_SET_TIMEOUT     = 43785
	NBD_SET_FLAGS       = 43786
	// enum
	NBD_CMD_READ  = 0
	NBD_CMD_WRITE = 1
	NBD_CMD_DISC  = 2
	NBD_CMD_FLUSH = 3
	NBD_CMD_TRIM  = 4
	// values for flags field
	NBD_FLAG_HAS_FLAGS  = (1 << 0) // nbd-server supports flags
	NBD_FLAG_READ_ONLY  = (1 << 1) // device is read-only
	NBD_FLAG_SEND_FLUSH = (1 << 2) // can flush writeback cache
	NBD_FLAG_SEND_FUA   = (1 << 3) // Send FUA (Force Unit Access)
	NBD_FLAG_ROTATIONAL = (1 << 4) // Use elevator algorithm - rotational media
	NBD_FLAG_SEND_TRIM  = (1 << 5) // Send TRIM (discard)

	// These are sent over the network in the request/reply magic fields
	NBD_REQUEST_MAGIC = 0x25609513
	NBD_REPLY_MAGIC   = 0x67446698
	// Do *not* use magics: 0x12560953 0x96744668.
)

// ioctl() helper function
func ioctl(a1, a2, a3 uintptr) (err error) {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, a1, a2, a3)
	if errno != 0 {
		err = errno
	}
	return err
}

// Device is our block interface; os.File is OK
type Device interface {
	// read-write
	ReadAt(b []byte, off int64) (n int, err error)
	WriteAt(b []byte, off int64) (n int, err error)
	// sync
	Sync() error
}

type request struct {
	magic  uint32
	typus  uint32
	handle uint64
	from   uint64
	len    uint32
}

type reply struct {
	magic  uint32
	error  uint32
	handle uint64
}

type NBD struct {
	device Device
	size   uint64
	nbd    *os.File
	socket int
}

func Create(device Device, size uint64) *NBD {
	return &NBD{device, size, nil, 0}
}

// return true if connected
func (nbd *NBD) IsConnected() bool {
	return nbd.nbd != nil && nbd.socket > 0
}

// get the size of the NBD
func (nbd *NBD) GetSize() uint64 {
	return nbd.size
}

// set the size of the NBD
func (nbd *NBD) Size(size uint64) (err error) {
	if err = ioctl(nbd.nbd.Fd(), NBD_SET_BLKSIZE, 4096); err != nil {
		err = &os.PathError{nbd.nbd.Name(), "ioctl NBD_SET_BLKSIZE", err}
	} else if err = ioctl(nbd.nbd.Fd(), NBD_SET_SIZE_BLOCKS, uintptr(size/4096)); err != nil {
		err = &os.PathError{nbd.nbd.Name(), "ioctl NBD_SET_SIZE_BLOCKS", err}
	}

	return err
}

// connect the network block device
func (nbd *NBD) Connect() (string, error) {
	pair, err := syscall.Socketpair(syscall.SOCK_STREAM, syscall.AF_UNIX, 0)

	if err != nil {
		return "", err
	}

	// find free nbd device
	var dev string
	for i := 0; ; i++ {
		dev = fmt.Sprintf("/dev/nbd%d", i)
		if _, err = os.Stat(dev); os.IsNotExist(err) {
			return "", fmt.Errorf("No free nbd devices (%v checked)", i)
		}
		if _, err = os.Stat(fmt.Sprintf("/sys/block/nbd%d/pid", i)); !os.IsNotExist(err) {
			continue // busy
		}

		if nbd.nbd, err = os.Open(dev); err == nil {
			// possible candidate
			ioctl(nbd.nbd.Fd(), BLKROSET, 0) // I'm really sorry about this
			if err := ioctl(nbd.nbd.Fd(), NBD_SET_SOCK, uintptr(pair[0])); err == nil {
				nbd.socket = pair[1]
				break // success
			}
		}
	}

	// set blk device size using ioctl
	if err = nbd.Size(nbd.size); err != nil {
		return "", err
	}

	// set ioctl flags
	if err = ioctl(nbd.nbd.Fd(), NBD_SET_FLAGS, 1); err != nil {
		return "", &os.PathError{nbd.nbd.Name(), "ioctl NBD_SET_FLAGS", err}
	}

	go nbd.handle()
	return dev, err
}

func (nbd *NBD) Wait() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var err error

	// NBD_DO_IT does not return until disconnect
	err = ioctl(nbd.nbd.Fd(), NBD_DO_IT, 0)
	if err != nil {
		return &os.PathError{nbd.nbd.Name(), "ioctl NBD_DO_IT", err}
	}

	err = ioctl(nbd.nbd.Fd(), NBD_DISCONNECT, 0)
	if err != nil {
		return &os.PathError{nbd.nbd.Name(), "ioctl NBD_DISCONNECT", err}
	}

	err = ioctl(nbd.nbd.Fd(), NBD_CLEAR_SOCK, 0)
	if err != nil {
		return &os.PathError{nbd.nbd.Name(), "ioctl NBD_CLEAR_SOCK", err}
	}

	return nil
}

// handle requests
func (nbd *NBD) handle() {
	buf := make([]byte, 2<<19)
	var x request

	for {
		syscall.Read(nbd.socket, buf[0:28])

		x.magic = binary.BigEndian.Uint32(buf)
		x.typus = binary.BigEndian.Uint32(buf[4:8])
		x.handle = binary.BigEndian.Uint64(buf[8:16])
		x.from = binary.BigEndian.Uint64(buf[16:24])
		x.len = binary.BigEndian.Uint32(buf[24:28])

		switch x.magic {
		case NBD_REPLY_MAGIC:
			fallthrough
		case NBD_REQUEST_MAGIC:
			switch x.typus {
			case NBD_CMD_READ:
				nbd.device.ReadAt(buf[16:16+x.len], int64(x.from))
				binary.BigEndian.PutUint32(buf[0:4], NBD_REPLY_MAGIC)
				binary.BigEndian.PutUint32(buf[4:8], 0)
				syscall.Write(nbd.socket, buf[0:16+x.len])
			case NBD_CMD_WRITE:
				n, _ := syscall.Read(nbd.socket, buf[28:28+x.len])
				for uint32(n) < x.len {
					m, _ := syscall.Read(nbd.socket, buf[28+n:28+x.len])
					n += m
				}
				nbd.device.WriteAt(buf[28:28+x.len], int64(x.from))
				binary.BigEndian.PutUint32(buf[0:4], NBD_REPLY_MAGIC)
				binary.BigEndian.PutUint32(buf[4:8], 0)
				syscall.Write(nbd.socket, buf[0:16])
			case NBD_CMD_DISC:
				panic("Disconnect")
			case NBD_CMD_FLUSH:
				nbd.device.Sync()
			case NBD_CMD_TRIM:
				binary.BigEndian.PutUint32(buf[0:4], NBD_REPLY_MAGIC)
				binary.BigEndian.PutUint32(buf[4:8], 1)
				syscall.Write(nbd.socket, buf[0:16])
			default:
				panic("unknown command")
			}
		default:
			panic("Invalid packet")
		}
	}
}
