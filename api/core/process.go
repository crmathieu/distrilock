package core

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"sync"
	"syscall"

	"bitbucket.org/gdm85/go-distrilock/api"
)

const lockExt = ".lck"

var (
	knownResources     = map[string]*os.File{}
	resourceAcquiredBy = map[*os.File]*net.TCPConn{}
	knownResourcesLock sync.RWMutex
	validLockNameRx    = regexp.MustCompile(`^[A-Za-z0-9.\-]+$`)
)

func ProcessRequest(directory string, client *net.TCPConn, req api.LockRequest) api.LockResponse {
	var res api.LockResponse
	res.LockRequest = req
	// override with own version
	res.VersionMajor, res.VersionMinor = api.VersionMajor, api.VersionMinor

	// validate lock name
	if !validLockNameRx.MatchString(req.LockName) {
		res.Result = api.BadRequest
		res.Reason = "invalid lock name"
		return res
	}

	switch res.Command {
	case api.Acquire:
		res.Result, res.Reason = acquire(client, req.LockName, directory)
	case api.Release:
		res.Result, res.Reason = release(client, req.LockName, directory)
	case api.Peek:
		res.Result, res.Reason, res.IsLocked = peek(req.LockName, directory)
	case api.Verify:
		res.Result, res.Reason = verifyOwnership(client, req.LockName, directory)
	default:
		res.Result = api.BadRequest
		res.Reason = "unknown command"
	}

	return res
}

func ProcessDisconnect(client *net.TCPConn) {
	knownResourcesLock.Lock()

	var filesToDrop []*os.File

	// perform (inefficient) reverse lookups for deletions
	for f, by := range resourceAcquiredBy {
		if by == client {
			_ = f.Close()
			filesToDrop = append(filesToDrop, f)
			delete(resourceAcquiredBy, f)
		}
	}
	for _, droppedF := range filesToDrop {
		for name, f := range knownResources {
			if f == droppedF {
				delete(knownResources, name)
				break
			}
		}
	}

	knownResourcesLock.Unlock()
}

func shortAcquire(client *net.TCPConn, f *os.File, fullLock bool) (api.LockCommandResult, string) {
	// check if lock was acquired by a different client
	by, ok := resourceAcquiredBy[f]
	if fullLock {
		knownResourcesLock.Unlock()
	} else {
		knownResourcesLock.RUnlock()
	}
	if !ok {
		panic("BUG: missing resource acquired by record")
	}
	if by != client {
		return api.Failed, "resource acquired through a different session"
	}

	// already acquired by self
	//TODO: this is a no-operation, should lock be acquired again with fcntl?
	//		and what if the re-acquisition fails? that would perhaps qualify
	//		as a different lock command?
	return api.Success, "no-op"
}

func acquire(client *net.TCPConn, lockName, directory string) (api.LockCommandResult, string) {
	knownResourcesLock.RLock()

	f, ok := knownResources[lockName]
	if ok {
		return shortAcquire(client, f, false)
	}
	knownResourcesLock.RUnlock()
	knownResourcesLock.Lock()

	// check again, as meanwhile lock could have been created
	f, ok = knownResources[lockName]
	if ok {
		return shortAcquire(client, f, true)
	}

	var err error
	f, err = os.OpenFile(directory+lockName+lockExt, os.O_RDWR|os.O_CREATE, 0664)
	if err != nil {
		knownResourcesLock.Unlock()

		return api.InternalError, err.Error()
	}

	err = acquireLockDirect(f)
	if err != nil {
		f.Close()
		knownResourcesLock.Unlock()

		if e, ok := err.(syscall.Errno); ok {
			if e == syscall.EAGAIN || e == syscall.EACCES { // to be POSIX-compliant, both errors must be checked
				return api.Failed, "resource acquired by different process"
			}
		}

		return api.InternalError, err.Error()
	}

	_, err = f.Write([]byte(fmt.Sprintf("locked by %v", client.RemoteAddr())))
	if err != nil {
		f.Close()
		knownResourcesLock.Unlock()

		return api.InternalError, err.Error()
	}

	resourceAcquiredBy[f] = client
	knownResources[lockName] = f
	knownResourcesLock.Unlock()

	// successful lock acquire
	return api.Success, ""
}

func peek(lockName, directory string) (api.LockCommandResult, string, bool) {
	knownResourcesLock.RLock()
	defer knownResourcesLock.RUnlock()

	f, ok := knownResources[lockName]
	if ok {
		//TODO: perhaps check that file is really UNLCK?
		return api.Success, "", true
	}
	var err error
	// differently from acquire(), file must exist here
	f, err = os.OpenFile(directory+lockName+lockExt, os.O_RDONLY, 0664)
	if err != nil {
		if e, ok := err.(*os.PathError); ok {
			if e.Err == syscall.ENOENT {
				return api.Success, "", false
			}
		}
		return api.InternalError, err.Error(), false
	}

	isUnlocked, err := isUnlocked(f)
	_ = f.Close()
	if err != nil {
		return api.InternalError, err.Error(), false
	}

	return api.Success, "", !isUnlocked
}

func release(client *net.TCPConn, lockName, directory string) (api.LockCommandResult, string) {
	knownResourcesLock.RLock()

	f, ok := knownResources[lockName]
	if !ok {
		knownResourcesLock.RUnlock()
		return api.Failed, "lock not found"
	}

	// check if lock was acquired by a different client
	by, ok := resourceAcquiredBy[f]
	if !ok {
		panic("BUG: missing resource acquired by record")
	}
	if by != client {
		knownResourcesLock.RUnlock()
		return api.Failed, "resource acquired through a different session"
	}
	knownResourcesLock.RUnlock()
	knownResourcesLock.Lock()

	f, ok = knownResources[lockName]
	if !ok {
		knownResourcesLock.Unlock()
		return api.Failed, "lock not found"
	}

	// check if lock was acquired by a different client
	by, ok = resourceAcquiredBy[f]
	if !ok {
		panic("BUG: missing resource acquired by record")
	}
	if by != client {
		knownResourcesLock.Unlock()
		return api.Failed, "resource acquired through a different session"
	}

	err := releaseLock(f)
	if err != nil {
		knownResourcesLock.Unlock()
		return api.InternalError, err.Error()
	}

	delete(knownResources, lockName)
	delete(resourceAcquiredBy, f)
	_ = f.Close()
	err = os.Remove(directory + lockName + lockExt)

	knownResourcesLock.Unlock()

	if err != nil {
		return api.InternalError, err.Error()
	}

	return api.Success, ""
}

// verifyOwnership verifies that specified client has acquired lock through this node.
func verifyOwnership(client *net.TCPConn, lockName, directory string) (api.LockCommandResult, string) {
	knownResourcesLock.RLock()

	f, ok := knownResources[lockName]
	if !ok {
		knownResourcesLock.RUnlock()
		return api.Failed, "lock not found"
	}

	// check if lock was acquired by a different client
	by, ok := resourceAcquiredBy[f]
	knownResourcesLock.RUnlock()
	if !ok {
		panic("BUG: missing resource acquired by record")
	}
	if by != client {
		return api.Failed, "resource acquired through a different session"
	}
	knownResourcesLock.Lock()
	f, ok = knownResources[lockName]
	if !ok {
		knownResourcesLock.Unlock()
		return api.Failed, "lock not found"
	}

	// check if lock was acquired by a different client
	by, ok = resourceAcquiredBy[f]
	if !ok {
		panic("BUG: missing resource acquired by record")
	}
	if by != client {
		knownResourcesLock.Unlock()
		return api.Failed, "resource acquired through a different session"
	}

	// lock was already acquired by self
	// thus re-acquiring lock must succeed
	err := acquireLockDirect(f)
	knownResourcesLock.Unlock()
	if err != nil {
		if e, ok := err.(syscall.Errno); ok {
			if e == syscall.EAGAIN || e == syscall.EACCES { // to be POSIX-compliant, both errors must be checked
				return api.Failed, "resource acquired by different process"
			}
		}

		return api.InternalError, err.Error()
	}

	// successful lock re-acquisition
	return api.Success, ""
}