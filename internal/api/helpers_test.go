package api

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"testing"
)

type formPart struct {
	name     string
	filename string
	value    string
}

// multipartBody builds a deterministic multipart body and its content type.
func multipartBody(t *testing.T, parts ...formPart) (string, string) {
	t.Helper()
	buffer := new(bytes.Buffer)
	writer := multipart.NewWriter(buffer)
	for _, part := range parts {
		header := make(textproto.MIMEHeader)
		if part.filename != "" {
			header.Set("Content-Disposition",
				`form-data; name="`+part.name+`"; filename="`+part.filename+`"`)
			header.Set("Content-Type", "text/csv")
		} else {
			header.Set("Content-Disposition", `form-data; name="`+part.name+`"`)
		}
		destination, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := destination.Write([]byte(part.value)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buffer.String(), writer.FormDataContentType()
}

// rawMultipartBody builds a body from preformatted multipart content with the
// given boundary. It is used to create duplicate and unknown parts.
func rawMultipartBody(boundary string, parts [][2]string) string {
	buffer := new(bytes.Buffer)
	for _, part := range parts {
		buffer.WriteString("--" + boundary + "\r\n")
		buffer.WriteString(`Content-Disposition: form-data; name="` + part[0] + `"` + "\r\n\r\n")
		buffer.WriteString(part[1])
		buffer.WriteString("\r\n")
	}
	buffer.WriteString("--" + boundary + "--\r\n")
	return buffer.String()
}
