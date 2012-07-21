// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Package inotify implements a wrapper for the Linux inotify system.

Example:
    watcher, err := inotify.NewWatcher()
    if err != nil {
        log.Fatal(err)
    }
    err = watcher.Watch("/tmp")
    if err != nil {
        log.Fatal(err)
    }
    for {
        select {
        case ev := <-watcher.Event:
            log.Println("event:", ev)
        case err := <-watcher.Error:
            log.Println("error:", err)
        }
    }

*/
package inotify

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

type Event struct {
	Mask   uint32 // Mask of events
	Cookie uint32 // Unique cookie associating related events (for rename(2))
	Name   string // File name (optional)
}

type watch struct {
	wd    uint32 // Watch descriptor (as returned by the inotify_add_watch() syscall)
	flags uint32 // inotify flags of this watch (see inotify(7) for the list of valid flags)
}

type Watcher struct {
	mu       sync.Mutex
	fd       int               // File descriptor (as returned by the inotify_init() syscall)
	watches  map[string]*watch // Map of inotify watches (key: path)
	paths    map[int]string    // Map of watched paths (key: watch descriptor)
	Error    chan error        // Errors are sent on this channel
	Event    chan *Event       // Events are returned on this channel
	done     chan bool         // Channel for sending a "quit message" to the reader goroutine
	isClosed bool              // Set to true when Close() is first called
}

// NewWatcher creates and returns a new inotify instance using inotify_init(2)
func NewWatcher() (*Watcher, error) {
	fd, errno := syscall.InotifyInit()
	if fd == -1 {
		return nil, os.NewSyscallError("inotify_init", errno)
	}
	w := &Watcher{
		fd:      fd,
		watches: make(map[string]*watch),
		paths:   make(map[int]string),
		Event:   make(chan *Event),
		Error:   make(chan error),
		done:    make(chan bool, 1),
	}

	go w.readEvents()
	return w, nil
}

// Close closes an inotify watcher instance
// It sends a message to the reader goroutine to quit and removes all watches
// associated with the inotify instance
func (w *Watcher) Close() error {
	if w.isClosed {
		return nil
	}
	w.isClosed = true

	// Send "quit" message to the reader goroutine
	w.done <- true

	return nil
}

// AddWatch adds path to the watched file set.
// The flags are interpreted as described in inotify_add_watch(2).
func (w *Watcher) AddWatch(path string, flags uint32) error {
	if w.isClosed {
		return errors.New("inotify instance already closed")
	}

	watchEntry, found := w.watches[path]
	if found {
		watchEntry.flags |= flags
		flags |= syscall.IN_MASK_ADD
	}

	w.mu.Lock() // synchronize with readEvents goroutine
	defer w.mu.Unlock()

	wd, err := syscall.InotifyAddWatch(w.fd, path, flags)
	if err != nil {
		return &os.PathError{
			Op:   "inotify_add_watch",
			Path: path,
			Err:  err,
		}
	}

	if !found {
		w.watches[path] = &watch{wd: uint32(wd), flags: flags}
		w.paths[wd] = path
	}
	return nil
}

// Watch adds path to the watched file set, watching all events.
func (w *Watcher) Watch(path string) error {
	return w.AddWatch(path, IN_ALL_EVENTS)
}

// RemoveWatch removes path from the watched file set.
func (w *Watcher) RemoveWatch(path string) error {
	if w.isClosed {
		return errors.New(fmt.Sprintf("can't remove non-existent inotify watch for: %s", path))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	watch, ok := w.watches[path]
	if !ok {
		return errors.New(fmt.Sprintf("can't remove non-existent inotify watch for: %s", path))
	}
	success, errno := syscall.InotifyRmWatch(w.fd, watch.wd)
	if success == -1 {
		return os.NewSyscallError("inotify_rm_watch", errno)
	}
	delete(w.watches, path)
	delete(w.paths, int(watch.wd))
	return nil
}

func newFdSet(fd int) *syscall.FdSet {
	fds := new(syscall.FdSet)
	elemSize := 32 * 32 / len(fds.Bits)
	fds.Bits[fd/elemSize] |= 1 << uint(fd%elemSize)
	return fds
}

// readEvents reads from the inotify file descriptor, converts the
// received events into Event objects and sends them via the Event channel
func (w *Watcher) readEvents() {
	var buf [syscall.SizeofInotifyEvent * 4096]byte

	// Timeout after 500 milliseconds when waiting for events
	// so we can reliably close the Watcher
	timeout := int64(500e6)
	readFds := newFdSet(w.fd)
	for {
		var n int
		var err error
		select {
		// See if there is a message on the "done" channel
		case <-w.done:
		// Otherwise select fd with timeout
		default:
			tmpSet := *readFds
			timeval := syscall.NsecToTimeval(timeout)
			n, err = syscall.Select(w.fd+1, &tmpSet, nil, nil, &timeval)
			if n == 1 {
				n, err = syscall.Read(w.fd, buf[0:])
			} else if err != nil {
				w.Error <- err
			} else {
				continue
			}
		}

		// If EOF or a "done" message is received
		if n == 0 {
			goto done
		}
		if n < 0 {
			w.Error <- os.NewSyscallError("read", err)
			continue
		}
		if n < syscall.SizeofInotifyEvent {
			w.Error <- errors.New("inotify: short read in readEvents()")
			continue
		}

		var offset uint32 = 0
		// We don't know how many events we just read into the buffer
		// While the offset points to at least one whole event...
		for offset <= uint32(n-syscall.SizeofInotifyEvent) {
			// Point "raw" to the event in the buffer
			raw := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			event := new(Event)
			event.Mask = uint32(raw.Mask)
			event.Cookie = uint32(raw.Cookie)
			nameLen := uint32(raw.Len)
			// If the event happened to the watched directory or the watched file, the kernel
			// doesn't append the filename to the event, but we would like to always fill the
			// the "Name" field with a valid filename. We retrieve the path of the watch from
			// the "paths" map.
			w.mu.Lock()
			event.Name = w.paths[int(raw.Wd)]
			// Check if the the watch was removed
			if event.Mask&IN_IGNORED != 0 {
				// remove stale watch
				delete(w.watches, event.Name)
				delete(w.paths, int(raw.Wd))
			}
			w.mu.Unlock()
			if nameLen > 0 {
				// Point "bytes" at the first byte of the filename
				bytes := (*[syscall.PathMax]byte)(unsafe.Pointer(&buf[offset+syscall.SizeofInotifyEvent]))
				// The filename is padded with NUL bytes. TrimRight() gets rid of those.
				event.Name += "/" + strings.TrimRight(string(bytes[0:nameLen]), "\000")
			}
			// Send the event on the events channel
			w.Event <- event

			// Move to the next event in the buffer
			offset += syscall.SizeofInotifyEvent + nameLen
		}
	}
done:
	w.isClosed = true // keep API behaviour consistent when EOF was read
	err := syscall.Close(w.fd)
	if err != nil {
		w.Error <- os.NewSyscallError("close", err)
	}
	close(w.Event)
	close(w.Error)
	for path, watch := range w.watches {
		delete(w.watches, path)
		delete(w.paths, int(watch.wd))
	}
}

// String formats the event e in the form
// "filename: 0xEventMask = IN_ACCESS|IN_ATTRIB_|..."
func (e *Event) String() string {
	var events string = ""

	m := e.Mask
	for _, b := range eventBits {
		if m&b.Value != 0 {
			m &^= b.Value
			events += "|" + b.Name
		}
	}

	if m != 0 {
		events += fmt.Sprintf("|%#x", m)
	}
	if len(events) > 0 {
		events = " == " + events[1:]
	}

	return fmt.Sprintf("%q: %#x%s", e.Name, e.Mask, events)
}

const (
	// Options for inotify_init() are not exported
	// IN_CLOEXEC    uint32 = syscall.IN_CLOEXEC
	// IN_NONBLOCK   uint32 = syscall.IN_NONBLOCK

	// Options for AddWatch
	IN_DONT_FOLLOW uint32 = syscall.IN_DONT_FOLLOW
	IN_ONESHOT     uint32 = syscall.IN_ONESHOT
	IN_ONLYDIR     uint32 = syscall.IN_ONLYDIR

	// The "IN_MASK_ADD" option is not exported, as AddWatch
	// adds it automatically, if there is already a watch for the given path
	// IN_MASK_ADD      uint32 = syscall.IN_MASK_ADD

	// Events
	IN_ACCESS        uint32 = syscall.IN_ACCESS
	IN_ALL_EVENTS    uint32 = syscall.IN_ALL_EVENTS
	IN_ATTRIB        uint32 = syscall.IN_ATTRIB
	IN_CLOSE         uint32 = syscall.IN_CLOSE
	IN_CLOSE_NOWRITE uint32 = syscall.IN_CLOSE_NOWRITE
	IN_CLOSE_WRITE   uint32 = syscall.IN_CLOSE_WRITE
	IN_CREATE        uint32 = syscall.IN_CREATE
	IN_DELETE        uint32 = syscall.IN_DELETE
	IN_DELETE_SELF   uint32 = syscall.IN_DELETE_SELF
	IN_MODIFY        uint32 = syscall.IN_MODIFY
	IN_MOVE          uint32 = syscall.IN_MOVE
	IN_MOVED_FROM    uint32 = syscall.IN_MOVED_FROM
	IN_MOVED_TO      uint32 = syscall.IN_MOVED_TO
	IN_MOVE_SELF     uint32 = syscall.IN_MOVE_SELF
	IN_OPEN          uint32 = syscall.IN_OPEN

	// Special events
	IN_ISDIR      uint32 = syscall.IN_ISDIR
	IN_IGNORED    uint32 = syscall.IN_IGNORED
	IN_Q_OVERFLOW uint32 = syscall.IN_Q_OVERFLOW
	IN_UNMOUNT    uint32 = syscall.IN_UNMOUNT
)

var eventBits = []struct {
	Value uint32
	Name  string
}{
	{IN_ACCESS, "IN_ACCESS"},
	{IN_ATTRIB, "IN_ATTRIB"},
	{IN_CLOSE, "IN_CLOSE"},
	{IN_CLOSE_NOWRITE, "IN_CLOSE_NOWRITE"},
	{IN_CLOSE_WRITE, "IN_CLOSE_WRITE"},
	{IN_CREATE, "IN_CREATE"},
	{IN_DELETE, "IN_DELETE"},
	{IN_DELETE_SELF, "IN_DELETE_SELF"},
	{IN_MODIFY, "IN_MODIFY"},
	{IN_MOVE, "IN_MOVE"},
	{IN_MOVED_FROM, "IN_MOVED_FROM"},
	{IN_MOVED_TO, "IN_MOVED_TO"},
	{IN_MOVE_SELF, "IN_MOVE_SELF"},
	{IN_OPEN, "IN_OPEN"},
	{IN_ISDIR, "IN_ISDIR"},
	{IN_IGNORED, "IN_IGNORED"},
	{IN_Q_OVERFLOW, "IN_Q_OVERFLOW"},
	{IN_UNMOUNT, "IN_UNMOUNT"},
}
