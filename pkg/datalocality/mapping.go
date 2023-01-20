package datalocality

import (
	"strings"
)

type DataLocality uint

const (
	None				DataLocality = iota
	Write_preferLocalDc
)

// DataLocality -> String
var dataLocalityStringMap = []string {
	"none",
	"write_preferlocaldc",
}
func (d DataLocality) String() string {
	return dataLocalityStringMap[d]
}

// String -> DataLocality
var stringDataLocalityMap = map[string]DataLocality {
	"none": None,
	"write_preferlocaldc": Write_preferLocalDc,
}
func FromString(s string) (DataLocality, bool) {
	value, ok := stringDataLocalityMap[strings.ToLower(s)]
	return value, ok
}
