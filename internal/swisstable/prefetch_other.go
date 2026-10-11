//go:build !linux

package swisstable

func (m *SwissPositionMap) Prefetch(int, func(int) [32]byte) {}
