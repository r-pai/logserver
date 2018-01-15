package parse

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Log struct {
	Msg      string     `json:"msg"`
	Level    string     `json:"level"`
	Time     *time.Time `json:"time,omitempty"`
	FS       string     `json:"fs"`
	FileName string     `json:"file_name"`
	Line     int        `json:"line"`
	Offset   int        `json:"offset"`
}

func (l *Log) parseTime(timeFormats []string, timeString string) {
	timeString = strings.Replace(timeString, ",", ".", -1)
	for _, timeFormat := range timeFormats {
		switch timeFormat {
		case "unix_float":
			if f, err := strconv.ParseFloat(timeString, 64); err != nil {
				tt := time.Unix(int64(f), int64(f-float64(int64(f))))
				l.Time = &tt
				return
			}
		case "unix_int":
			if i, err := strconv.ParseInt(timeString, 10, 64); err != nil {
				tt := time.Unix(i, 0)
				l.Time = &tt
				return
			}
		default:
			t, err := time.Parse(timeFormat, timeString)
			if err == nil {
				l.Time = &t
				return
			}
		}
	}
}

var keyword = regexp.MustCompile(`(%\(([^)]+\))[diouxXeEfFgGcrs])`)

func (l *Log) injectArgs(args interface{}) {
	l.Msg = strings.Replace(l.Msg, "%s", "%v", -1)

	switch args := args.(type) {
	case []interface{}:
		l.Msg = fmt.Sprintf(l.Msg, args...)
	case map[string]interface{}:
		l.Msg = keyword.ReplaceAllStringFunc(l.Msg, func(src string) string {
			key := src[2 : len(src)-2]
			val, ok := args[key]
			if !ok {
				return src
			}
			return fmt.Sprintf("%v", val)
		})
	case string:
		var obj interface{}
		if err := json.Unmarshal([]byte(args), &obj); err != nil {
			l.injectArgs(obj)
		}
	}
}