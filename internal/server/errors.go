package server

import (
	"encoding/json"
	"net/http"
)

// writeOpenAIError 按官方 SDK 可解析的格式输出错误：
//
//	{"error":{"message":..,"type":..,"param":null,"code":..}}
func (s *Server) writeOpenAIError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    errType,
			"param":   nil,
			"code":    errType,
		},
	})
}
