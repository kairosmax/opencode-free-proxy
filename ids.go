package main

import (
	"crypto/rand"
	"fmt"
	"time"
)

const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// mintOpencodeID reproduces opencode's Identifier.create ascending scheme:
// prefix + "_" + hex(6 low bytes of ts<<12|counter) + 14 base62 chars.
func mintOpencodeID(prefix string) string {
	now := (uint64(time.Now().UnixMilli()) << 12) | 1
	var tb [6]byte
	for i := 0; i < 6; i++ {
		tb[i] = byte(now >> (40 - 8*i))
	}
	return fmt.Sprintf("%s_%s%s", prefix, fmt.Sprintf("%x", tb[:]), randomBase62(14))
}

func randomBase62(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[b[i]%62]
	}
	return string(b)
}
