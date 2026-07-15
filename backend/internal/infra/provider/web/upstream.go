package web

import "net/http"

func responseUpstreamURL(response *http.Response) string {
	if response == nil || response.Request == nil || response.Request.URL == nil {
		return ""
	}
	return response.Request.URL.String()
}
