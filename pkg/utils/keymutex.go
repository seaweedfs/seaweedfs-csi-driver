package utils

import "sync"

// KeyMutex provides a lock per key to serialize operations per volume.
type KeyMutex struct {
	mutexes sync.Map
}

func NewKeyMutex() *KeyMutex {
	return &KeyMutex{}
}

func (km *KeyMutex) Get(key string) *sync.Mutex {
	m, _ := km.mutexes.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (km *KeyMutex) Delete(key string) {
	km.mutexes.Delete(key)
}
