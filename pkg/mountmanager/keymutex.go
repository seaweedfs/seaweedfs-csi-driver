package mountmanager

import "sync"

// keyMutex provides a lock per key to serialize operations per volume.
type keyMutex struct {
	mutexes sync.Map
}

func newKeyMutex() *keyMutex {
	return &keyMutex{}
}

func (km *keyMutex) get(key string) *sync.Mutex {
	m, _ := km.mutexes.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (km *keyMutex) delete(key string) {
	km.mutexes.Delete(key)
}
