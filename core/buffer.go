package core

import (
	"crypto/rand"
	"sync"

	"github.com/awnumar/memguard/crypto"
	"github.com/awnumar/memguard/memcall"
)

var (
	buffers = new(BufferList)
)

/*
Buffer is a structure that holds raw sensitive data.

The number of Buffers that can exist at one time is limited by how much memory your system's kernel allows each process to mlock/VirtualLock. Therefore you should call DestroyBuffer on Buffers that you no longer need, ideally defering a Destroy call after creating a new one.

If an API function that needs to edit a Buffer is given one that is immutable, the call will return an ErrImmutable. Similarly, if a function is given an Buffer that has been destroyed, the call will return an ErrDestroyed.
*/
type Buffer struct {
	sync.RWMutex // Local mutex lock

	alive   bool // Signals that destruction has not come
	mutable bool // Mutability state of underlying memory

	Data   []byte // Portion of memory holding the data
	memory []byte // Entire allocated memory region

	preguard  []byte // Guard page addressed before the data
	inner     []byte // Inner region between the guard pages
	postguard []byte // Guard page addressed after the data

	canaryval []byte // Value written behind data to detect spillage
	canaryref []byte // Protected reference value for the canary buffer
}

// BufferState encodes a buffer's various states.
type BufferState struct {
	IsMutable   bool // Is the memory writable?
	IsDestroyed bool // Has the buffer been destroyed?
}

/*
NewBuffer is a raw constructor for the Buffer object.
*/
func NewBuffer(size int) (*Buffer, error) {
	var err error

	// Return an error if length < 1.
	if size < 1 {
		return nil, ErrInvalidLength
	}

	// Declare and allocate
	b := new(Buffer)

	// Allocate the total needed memory
	innerLen := roundToPageSize(size + 32)
	b.memory, err = memcall.Alloc((2 * pageSize) + innerLen)
	if err != nil {
		Panic(err)
	}

	// Construct slice reference for data buffer.
	b.Data = getBytes(&b.memory[pageSize+innerLen-size], size)

	// Construct slice references for page sectors.
	b.preguard = getBytes(&b.memory[0], pageSize)
	b.inner = getBytes(&b.memory[pageSize], innerLen)
	b.postguard = getBytes(&b.memory[pageSize+innerLen], pageSize)

	// Construct slice references for canary sectors.
	b.canaryref = getBytes(&b.memory[pageSize-32], 32)
	b.canaryval = getBytes(&b.memory[pageSize+innerLen-size-32], 32)

	// Lock the pages that will hold sensitive data.
	if err := memcall.Lock(b.inner); err != nil {
		Panic(err)
	}

	// Populate the canary values with fresh random bytes.
	if _, err := rand.Read(b.canaryref); err != nil {
		Panic(err)
	}
	crypto.Copy(b.canaryval, b.canaryref)

	// Make the guard pages inaccessible.
	if err := memcall.Protect(b.preguard, memcall.NoAccess); err != nil {
		Panic(err)
	}
	if err := memcall.Protect(b.postguard, memcall.NoAccess); err != nil {
		Panic(err)
	}

	// Set remaining properties
	b.alive = true
	b.mutable = true

	// Append the container to list of active buffers.
	buffers.Add(b)

	// Return the created Buffer to the caller.
	return b, nil
}

/*
GetBufferState returns a BufferState struct that encodes state information about a given Buffer object. It exports two fields, IsMutable and IsDestroyed, which signify whether the Buffer is mutable and whether it has been destroyed, respectively.
*/
func GetBufferState(b *Buffer) BufferState {
	b.RLock()
	defer b.RUnlock()
	return BufferState{IsMutable: b.mutable, IsDestroyed: !b.alive}
}

// Freeze makes the underlying memory of a given buffer immutable.
func Freeze(b *Buffer) error {
	// Attain lock.
	b.Lock()
	defer b.Unlock()

	// Check if destroyed.
	if !b.alive {
		return ErrDestroyed
	}

	// Only do anything if currently mutable.
	if b.mutable {
		// Make the memory immutable.
		if err := memcall.Protect(b.inner, memcall.ReadOnly); err != nil {
			Panic(err)
		}
		b.mutable = false
	}

	return nil
}

// Melt makes the underlying memory of a given buffer mutable.
func Melt(b *Buffer) error {
	// Attain lock.
	b.Lock()
	defer b.Unlock()

	// Check if destroyed.
	if !b.alive {
		return ErrDestroyed
	}

	// Only do anything if currently immutable.
	if !b.mutable {
		// Make the memory mutable.
		if err := memcall.Protect(b.inner, memcall.ReadWrite); err != nil {
			Panic(err)
		}
		b.mutable = true
	}

	return nil
}

/*
DestroyBuffer performs some security checks, securely wipes the contents of, and then releases a Buffer's memory back to the OS. If a security check fails, the process will attempt to wipe all it can before safely panicking.

This function should be called on all Buffers before exiting. DestroyAll is designed for this purpose, as is CatchInterrupt and SafeExit. We recommend using a combination of them as suited to your program.

If the Buffer has already been destroyed, subsequent calls are idempotent.
*/
func DestroyBuffer(b *Buffer) {
	// Attain a mutex lock on this Buffer.
	b.Lock()
	defer b.Unlock()

	// Return if it's already destroyed.
	if !b.alive {
		return
	}

	// Make all of the memory readable and writable.
	if err := memcall.Protect(b.memory, memcall.ReadWrite); err != nil {
		Panic(err)
	}

	// Verify the canary
	if !crypto.Equal(b.canaryval, b.canaryref) {
		Panic("<memguard::core::buffer> canary verification failed; buffer overflow detected")
	}

	// Remove this one from global slice.
	buffers.Remove(b)

	// Wipe the memory.
	crypto.MemClr(b.memory)

	// Unlock the pages that hold our data.
	if err := memcall.Unlock(b.inner); err != nil {
		Panic(err)
	}

	// Free all related memory.
	if err := memcall.Free(b.memory); err != nil {
		Panic(err)
	}

	// Reset the fields.
	b.alive = false
	b.mutable = false
	b.Data = nil
	b.memory = nil
	b.preguard = nil
	b.postguard = nil
	b.canaryref = nil
	b.canaryval = nil
}

// BufferList stores a list of buffers in a thread-safe manner.
type BufferList struct {
	sync.RWMutex
	list []*Buffer
}

// Add appends a given Buffer to the list.
func (l *BufferList) Add(b *Buffer) {
	l.Lock()
	defer l.Unlock()

	l.list = append(l.list, b)
}

// Remove removes a given Buffer from the list.
func (l *BufferList) Remove(b *Buffer) {
	l.Lock()
	defer l.Unlock()

	for i, v := range l.list {
		if v == b {
			l.list = append(l.list[:i], l.list[i+1:]...)
			break
		}
	}
}

// Exists checks if a given buffer is in the list.
func (l *BufferList) Exists(b *Buffer) bool {
	l.RLock()
	defer l.RUnlock()

	for _, v := range l.list {
		if b == v {
			return true
		}
	}

	return false
}

// Empty clears the list and returns its previous contents.
func (l *BufferList) Empty() []*Buffer {
	l.Lock()
	defer l.Unlock()

	list := make([]*Buffer, len(l.list))
	copy(list, l.list)

	l.list = nil

	return list
}