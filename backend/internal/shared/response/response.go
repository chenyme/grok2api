package response

import "github.com/gin-gonic/gin"

type successEnvelope struct {
	Data any `json:"data"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

// Success 返回 Admin API 统一成功包裹。
func Success(c *gin.Context, status int, data any) {
	c.JSON(status, successEnvelope{Data: data})
}

// Error 返回 Admin API 稳定错误码和请求 ID。
func Error(c *gin.Context, status int, code, message string) {
	requestID, _ := c.Get("requestId")
	c.AbortWithStatusJSON(status, errorEnvelope{Error: errorBody{Code: code, Message: message, RequestID: stringValue(requestID)}})
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
