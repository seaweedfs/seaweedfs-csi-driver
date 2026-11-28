package mountmanager

import "sync"

// keyMutex provides a lock per key, such as a volume ID or a target path.
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
