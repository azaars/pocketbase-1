package myonex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// postAPI makes a POST request to the specified URL with a JSON payload and returns the response body as a string.
func SendTAC(payload map[string]interface{}) (map[string]interface{}, error) {
	url := "https://rest.onexox.my/sendTAC"

	// Marshal the payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Create a new HTTP client
	client := &http.Client{}

	// Create a new HTTP POST request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers for the request
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Go-Client")

	// Perform the HTTP request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check for non-200 status codes
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("received non-200 status code: %d", resp.StatusCode)
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Unmarshal the response JSON into a map
	var responseMap map[string]interface{}
	err = json.Unmarshal(body, &responseMap)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal response JSON: %w", err)
	}

	return responseMap, nil
}
