// Package httpx 是 JSON HTTP 处理的小助手集。
package httpx

import (
	"encoding/json"
	"net/http"
)

// JSON 以指定状态码写 JSON 响应。
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Error 写 {"error": msg} 错误响应。
func Error(w http.ResponseWriter, code int, msg string) {
	JSON(w, code, map[string]string{"error": msg})
}

// Decode 解析请求 JSON body。
func Decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
