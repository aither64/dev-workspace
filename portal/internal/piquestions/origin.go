package piquestions

import (
	"reflect"
	"strconv"
)

const NativeArgumentLimit = 64 << 10

// DecodeOriginArguments rejects escaped lone UTF-16 surrogates as well as the
// bounded strict JSON rules. encoding/json alone would replace those strings.
func DecodeOriginArguments(data []byte, out any) error {
	if len(data) > NativeArgumentLimit {
		return ErrInvalid
	}
	quoted := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return ErrInvalid
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return ErrInvalid
		}
		n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return ErrInvalid
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return ErrInvalid
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return ErrInvalid
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return ErrInvalid
			}
			i += 6
		}
	}
	return Decode(data, out)
}

// VerifyNativeArguments compares retained immutable content, not original JSON
// byte spelling. Host numeric IDs are checked as indexes and never sent as args.
func VerifyNativeArguments(record QuestionRecord, arguments []byte) error {
	expected, err := canonical(PublishInput{Questions: record.Questions})
	if err != nil || !reflect.DeepEqual(expected, record.Questions) {
		return ErrConflict
	}
	var native struct {
		Questions []struct {
			Title   string   `json:"title"`
			Options []string `json:"options,omitempty"`
		} `json:"questions"`
	}
	if DecodeOriginArguments(arguments, &native) != nil || len(native.Questions) != len(expected) {
		return ErrConflict
	}
	for i, item := range native.Questions {
		q := expected[i]
		if !Text(item.Title, 4096) || item.Title != q.Title || len(item.Options) != len(q.Options) {
			return ErrConflict
		}
		for j, label := range item.Options {
			if !Text(label, 512) || label != q.Options[j].Label {
				return ErrConflict
			}
		}
	}
	return nil
}
