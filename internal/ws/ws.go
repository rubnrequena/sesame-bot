package ws

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type ErrorData struct {
	Status      string  `json:"status"`
	Message     string  `json:"message"`
	InstanceId  *string `json:"instanceId"`
	Explanation string  `json:"explanation"`
	ChatId      string  `json:"chatId"`
}

type Links struct {
	Self string `json:"self"`
}

type Response struct {
	Key struct {
		RemoteJid string `json:"remoteJid"`
		FromMe    bool   `json:"fromMe"`
		Id        string `json:"id"`
	} `json:"key"`
	PushName string `json:"pushName"`
	Status   string `json:"status"`
	Message  struct {
		Conversation string `json:"conversation"`
	} `json:"message"`
	ContextInfo struct {
		MentionedJid []string `json:"mentionedJid"`
	} `json:"contextInfo"`
	MessageType      string `json:"messageType"`
	MessageTimestamp int64  `json:"messageTimestamp"`
	InstanceId       string `json:"instanceId"`
	Source           string `json:"source"`
}

type Response401 struct {
	Status   int    `json:"status"`
	Error    string `json:"error"`
	Response struct {
		Message string `json:"message"`
	} `json:"response"`
}

type Response400 struct {
	Status   int    `json:"status"`
	Error    string `json:"error"`
	Response struct {
		Message [][]string `json:"message"`
	} `json:"response"`
}

type Payload struct {
	ChatId  string `json:"number"`
	Message string `json:"text"`
}

type ImagePayload struct {
	ChatId       string `json:"number"`
	MediaBase64  string `json:"media"`
	MediaCaption string `json:"caption"`
	MediaName    string `json:"medianame"`
	FileName     string `json:"fileName"`
	MimeType     string `json:"mimetype"`
	MediaType    string `json:"mediatype"`
}

// Whatsapp holds the configuration for the Evolution API client.
// BaseURL defaults to the EVOLUTION_BASE_URL env var if empty.
type Whatsapp struct {
	BaseURL    string
	Token      string
	Instance   string
	Name       string
	RetryDelay int
	MaxRetries int
}

func (w *Whatsapp) baseURL() string {
	if w.BaseURL != "" {
		return w.BaseURL
	}
	if u := os.Getenv("EVOLUTION_BASE_URL"); u != "" {
		return u
	}
	return "http://api2.sistemasrq.com:8080"
}

func (w *Whatsapp) SendMessage(contactNumber string, message string) (*Response, error) {
	url := fmt.Sprintf("%s/message/sendText/%s", w.baseURL(), w.Instance)
	nowFormat := time.Now().Format("2006-01-02 15:04:05")
	payloadMessage := fmt.Sprintf("[%s]\n_%s_\n%s", w.Name, nowFormat, message)
	jsonPayload := Payload{
		ChatId:  contactNumber,
		Message: payloadMessage,
	}
	jsonData, err := json.Marshal(jsonPayload)
	if err != nil {
		fmt.Println("Error marshalling JSON:", err)
		return nil, err
	}
	payload := strings.NewReader(string(jsonData))
	return w.send(url, payload)
}

func (w *Whatsapp) SendImage(contactNumber string, image string, name string, caption string) (*Response, error) {
	url := fmt.Sprintf("%s/message/sendMedia/%s", w.baseURL(), w.Instance)
	nowFormat := time.Now().Format("2006-01-02 15:04:05")
	payloadMessage := fmt.Sprintf("[%s]\n_%s_\n%s", w.Name, nowFormat, caption)
	jsonPayload := ImagePayload{
		ChatId:       contactNumber,
		MediaCaption: payloadMessage,
		MediaName:    name,
		MediaBase64:  image,
		FileName:     fmt.Sprintf("%s.png", name),
		MimeType:     "image/png",
		MediaType:    "image",
	}
	jsonData, err := json.Marshal(jsonPayload)
	if err != nil {
		fmt.Println("Error marshalling JSON:", err)
		return nil, err
	}
	result, err := w.send(url, strings.NewReader(string(jsonData)))
	if err != nil {
		fmt.Println("Error sending image:", err)
	}
	return result, err
}

func (w *Whatsapp) send(url string, payload io.Reader) (*Response, error) {
	if os.Getenv("EVOLUTION_ENABLED") != "true" {
		fmt.Println("WhatsApp (Evolution) is disabled")
		return nil, nil
	}

	maxRetries := w.MaxRetries
	retryDelay := time.Duration(w.RetryDelay) * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
		req, _ := http.NewRequest("POST", url, payload)
		req.Header.Add("accept", "application/json")
		req.Header.Add("content-type", "application/json")
		req.Header.Add("apiKey", w.Token)

		res, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error sending request (attempt %d/%d): %v\n", attempt, maxRetries, err)
			lastErr = err
			time.Sleep(retryDelay)
			continue
		}
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		if err != nil {
			fmt.Printf("Error reading response body (attempt %d/%d): %v\n", attempt, maxRetries, err)
			lastErr = err
			time.Sleep(retryDelay)
			continue
		}

		if res.StatusCode == 401 {
			var r401 Response401
			if err := json.Unmarshal(body, &r401); err != nil {
				lastErr = fmt.Errorf("error parsing 401: %w", err)
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("error sending message: %s", r401.Response.Message)
		}

		if res.StatusCode == 400 {
			var r400 Response400
			if err := json.Unmarshal(body, &r400); err != nil {
				lastErr = fmt.Errorf("error parsing 400: %w", err)
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("error sending message: %v", r400.Response.Message)
		}

		var response Response
		if err := json.Unmarshal(body, &response); err != nil {
			fmt.Printf("Error unmarshalling JSON (attempt %d/%d): %v\n", attempt, maxRetries, err)
			lastErr = err
			time.Sleep(retryDelay)
			continue
		}

		if response.Status != "PENDING" {
			lastErr = fmt.Errorf("unexpected status in response: %s", body)
			fmt.Printf("Error in response (attempt %d/%d): %v\n", attempt, maxRetries, lastErr)
			time.Sleep(retryDelay)
			continue
		}

		fmt.Printf("[WS OK] Status=[%s] ID=%s\n", response.Status, response.Key.Id)
		return &response, nil
	}

	return nil, fmt.Errorf("failed to send request after %d attempts: %v", maxRetries, lastErr)
}
