package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/reviactyl/agent/system"
)

// SuccessResponse wraps the successful response data with success and version fields.
type SuccessResponse struct {
	Success bool        `json:"success"`
	Version string      `json:"version"`
	Data    interface{} `json:"data,omitempty"`
}

// ErrorResponse wraps error responses with success and version fields.
type ErrorResponse struct {
	Success   bool        `json:"success"`
	Version   string      `json:"version"`
	Error     string      `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

// RespondSuccess sends a successful JSON response with success and version fields.
func RespondSuccess(c *gin.Context, status int, data interface{}) {
	c.JSON(status, SuccessResponse{
		Success: true,
		Version: system.Version,
		Data:    data,
	})
}

// RespondError sends an error JSON response with success and version fields.
func RespondError(c *gin.Context, status int, errMsg string) {
	reqID := c.Writer.Header().Get("X-Request-Id")
	c.JSON(status, ErrorResponse{
		Success:   false,
		Version:   system.Version,
		Error:     errMsg,
		RequestID: reqID,
	})
}
