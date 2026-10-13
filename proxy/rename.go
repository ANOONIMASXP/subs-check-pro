package proxies

import (
	"strconv"
	"sync"

	"github.com/sinspired/subs-check-pro/v3/config"
)

var (
	counter     = make(map[string]int)
	counterLock = sync.Mutex{}
)

func Rename(name, countryCodeTag string) string {
	flag := CountryCodeToFlag(name)

	key, label := name, name
	if config.Get().EnhancedTag {
		if countryCodeTag != "" {
			key, label = countryCodeTag, countryCodeTag
		} else if name != "" {
			// 添加 "ˣ" 角标, 例如: "HKˣ", 以做区分
			label = name + "ˣ"
			key = label
		}
	}

	counterLock.Lock()
	counter[key]++
	n := counter[key]
	counterLock.Unlock()

	return flag + label + "_" + strconv.Itoa(n)
}

// ResetRenameCounter 将所有计数器重置为 0
func ResetRenameCounter() {
	counterLock.Lock()
	defer counterLock.Unlock()

	counter = make(map[string]int)
}

func CountryCodeToFlag(code string) string {
	// 按 rune 计长：旧实现按字节判长，"é"（2 字节 1 rune）会通过检查，
	// 随后 []rune(code)[1] 越界 panic 整个改名阶段
	rs := []rune(code)
	if len(rs) != 2 {
		return "🏴‍☠"
	}

	// 转成大写（ASCII 位运算）
	upper0 := rs[0] &^ 0x20
	upper1 := rs[1] &^ 0x20

	r1 := upper0 - 'A' + 0x1F1E6
	r2 := upper1 - 'A' + 0x1F1E6

	return string([]rune{r1, r2})
}
