package healthcheck

import (
	"fmt"
	"strconv"
)

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func ftoa(value float64) string {
	return strconv.FormatFloat(value, 'f', 3, 64)
}
