// utils/path_ios.go
//go:build ios

package utils

import "os"

func GetExecutablePath() string {
    return os.TempDir()
}
