package inference

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type imageInputError struct {
	status  int
	code    string
	message string
}

func (image *imageEditJSONImage) UnmarshalJSON(data []byte) error {
	var url string
	if json.Unmarshal(data, &url) == nil {
		image.URL = url
		image.FileID = ""
		return nil
	}
	type rawImage imageEditJSONImage
	return json.Unmarshal(data, (*rawImage)(image))
}

func imageBadRequest(message string) *imageInputError {
	return &imageInputError{http.StatusBadRequest, "invalid_request", message}
}

// parseImageRequest accepts the official JSON shape and the multipart form
// emitted by clients when a user drops a local reference image into a prompt.
// The request body is already wrapped in MaxBytesReader by the caller.
func (h *Handler) parseImageRequest(c *gin.Context) (imageEditJSONRequest, []string, *imageInputError) {
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		return imageEditJSONRequest{}, nil, &imageInputError{http.StatusUnsupportedMediaType, "invalid_request", "图片请求支持 application/json 或 multipart/form-data"}
	}
	switch strings.ToLower(mediaType) {
	case "application/json":
		var request imageEditJSONRequest
		if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
			if isImageBodyTooLarge(err) {
				return imageEditJSONRequest{}, nil, &imageInputError{http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制"}
			}
			return imageEditJSONRequest{}, nil, imageBadRequest("图片 JSON 请求无效")
		}
		images := append([]imageEditJSONImage(nil), request.Images...)
		images = append(images, request.ReferenceImages...)
		if request.Image != nil {
			images = append([]imageEditJSONImage{*request.Image}, images...)
		}
		if len(images) > 8 {
			return imageEditJSONRequest{}, nil, imageBadRequest("image 或 images 数量必须在 1 到 8 之间")
		}
		urls := make([]string, 0, len(images))
		for _, image := range images {
			if strings.TrimSpace(image.FileID) != "" {
				return imageEditJSONRequest{}, nil, &imageInputError{http.StatusBadRequest, "unsupported_parameter", "当前暂不支持 image.file_id，请使用 image.url"}
			}
			if value := strings.TrimSpace(image.URL); value != "" {
				urls = append(urls, value)
			}
		}
		if len(urls) != len(images) {
			return imageEditJSONRequest{}, nil, imageBadRequest("每个 image 都必须提供有效 url")
		}
		return request, urls, nil
	case "multipart/form-data":
		boundary := params["boundary"]
		if boundary == "" {
			return imageEditJSONRequest{}, nil, imageBadRequest("multipart 请求缺少 boundary")
		}
		return h.parseMultipartImageRequest(multipart.NewReader(c.Request.Body, boundary))
	default:
		return imageEditJSONRequest{}, nil, &imageInputError{http.StatusUnsupportedMediaType, "invalid_request", "图片请求支持 application/json 或 multipart/form-data"}
	}
}

func (h *Handler) parseMultipartImageRequest(reader *multipart.Reader) (imageEditJSONRequest, []string, *imageInputError) {
	var request imageEditJSONRequest
	urls := make([]string, 0, 1)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return request, nil, imageMultipartError(err)
		}
		name := strings.ToLower(strings.TrimSpace(part.FormName()))
		if isImageFormField(name) {
			if len(urls) >= 8 {
				_ = part.Close()
				return request, nil, imageBadRequest("image 或 images 数量必须在 1 到 8 之间")
			}
			value, inputErr := h.readImagePart(part)
			_ = part.Close()
			if inputErr != nil {
				return request, nil, inputErr
			}
			urls = append(urls, value)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(part, (1<<20)+1))
		_ = part.Close()
		if readErr != nil {
			return request, nil, imageMultipartError(readErr)
		}
		if len(data) > 1<<20 {
			return request, nil, imageBadRequest("图片表单字段超过 1 MiB")
		}
		value := strings.TrimSpace(string(data))
		switch name {
		case "model":
			request.Model = value
		case "prompt":
			request.Prompt = value
		case "n":
			count, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				return request, nil, imageBadRequest("n 必须是整数")
			}
			request.Count = &count
		case "partial_images":
			count, parseErr := strconv.Atoi(value)
			if parseErr != nil {
				return request, nil, imageBadRequest("partial_images 必须是整数")
			}
			request.PartialImages = &count
		case "stream":
			stream, parseErr := strconv.ParseBool(value)
			if parseErr != nil {
				return request, nil, imageBadRequest("stream 必须是布尔值")
			}
			request.Stream = stream
		case "size":
			request.Size = value
		case "aspect_ratio":
			request.AspectRatio = value
		case "resolution":
			request.Resolution = value
		case "quality":
			request.Quality = value
		case "response_format":
			request.ResponseFormat = value
		case "storage_options":
			request.StorageOptions = json.RawMessage(`{"unsupported":true}`)
		}
	}
	return request, urls, nil
}

func isImageFormField(name string) bool {
	return name == "image" || name == "images" || name == "image[]" || name == "images[]" ||
		name == "reference_image" || name == "reference_image[]" || name == "reference_images" || name == "reference_images[]" ||
		strings.HasPrefix(name, "image[") || strings.HasPrefix(name, "images[") || strings.HasPrefix(name, "reference_image[") || strings.HasPrefix(name, "reference_images[")
}

func (h *Handler) readImagePart(part *multipart.Part) (string, *imageInputError) {
	data, err := io.ReadAll(io.LimitReader(part, h.maxBodyBytes+1))
	if err != nil {
		return "", imageMultipartError(err)
	}
	if int64(len(data)) > h.maxBodyBytes {
		return "", &imageInputError{http.StatusRequestEntityTooLarge, "request_too_large", "图片超过请求体大小限制"}
	}
	if part.FileName() == "" && !strings.HasPrefix(strings.ToLower(part.Header.Get("Content-Type")), "image/") {
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", imageBadRequest("image 不能为空")
		}
		return value, nil
	}
	if len(data) == 0 {
		return "", imageBadRequest("上传的 image 文件不能为空")
	}
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data)), nil
	default:
		return "", imageBadRequest("image 文件必须是 PNG、JPEG、WebP 或 GIF")
	}
}

func isImageBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func imageMultipartError(err error) *imageInputError {
	if isImageBodyTooLarge(err) {
		return &imageInputError{http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制"}
	}
	return imageBadRequest("图片 multipart 请求无效")
}
