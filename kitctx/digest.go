package kitctx

import (
	"crypto/sha256"
	"encoding/hex"
)

// Digest 返回 raw 的 SHA-256 十六进制（64 字符）。
//
// 只有一个实现点是刻意的：调用轨迹的摘要（`agent`）与剧本的幂等键（`playbook`）必须同口径，
// 各写一遍就会在换算法/换截断长度时悄悄漂移 —— 而幂等键一旦漂移，去重就变成"看起来没重复"。
// 需要短形态的调用方自己截前缀（如 `Digest(raw)[:12]`），截多长是使用方的事，不是第二份实现。
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
