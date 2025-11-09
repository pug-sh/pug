package clickhouse

func TextToString(val interface{}) (string, bool) {
	if val == nil {
		return "", false
	}

	str, ok := val.(string)
	if !ok {
		return "", false
	}

	return str, true
}

func StringToText(s string) interface{} {
	return s
}
