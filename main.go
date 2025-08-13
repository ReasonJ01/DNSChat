package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"
)

const cacheDuration = 1 * time.Hour

type cacheEntry struct {
	response  string
	expiresAt time.Time
}

var (
	cache      = make(map[string]cacheEntry)
	CacheMutex = &sync.RWMutex{}

	inFlightRequests = make(map[string]chan bool)
	inFlightMutex    = &sync.RWMutex{}
)

type dnsHandler struct{}

func getCache(q string) (string, bool) {
	CacheMutex.RLock()
	defer CacheMutex.RUnlock()
	res, ok := cache[q]
	if ok && time.Now().Before(res.expiresAt) {
		return res.response, true
	}
	return "", false
}

func setCache(q, res string) {
	CacheMutex.Lock()
	defer CacheMutex.Unlock()
	cache[q] = cacheEntry{
		response:  res,
		expiresAt: time.Now().Add(cacheDuration),
	}
}

func chunkString(s string, chunkSize int) []string {
	var chunks []string
	var buf []byte

	for i := 0; i < len(s); {
		_, sz := utf8.DecodeRuneInString(s[i:])
		if len(buf)+sz > chunkSize {
			chunks = append(chunks, string(buf))
			buf = buf[:0]
		}

		buf = append(buf, s[i:i+sz]...)
		i += sz
	}

	if len(buf) > 0 {
		chunks = append(chunks, string(buf))
	}

	return chunks
}

func cleanResponse(text string) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")

	return text
}

func getLLMResponse(q string) (string, error) {
	body := map[string]string{
		"model": "gpt-5-nano",
		"input": "Answer as quickly as possible and concisely max 3 sentences Use only A-Z, a-z, 0-9, and spaces, commas, periods, and question marks. No extra formatting.:" + q,
	}
	jsonBody, _ := json.Marshal(body)
	bodyReader := bytes.NewReader(jsonBody)
	r, err := http.NewRequest("POST", "https://api.openai.com/v1/responses", bodyReader)
	if err != nil {
		fmt.Println("Error creating request:", err)
		return "", err
	}

	r.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))

	client := &http.Client{}
	resp, err := client.Do(r)
	if err != nil {
		fmt.Println("Error sending request:", err)
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]any
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		fmt.Println("Error decoding response:", err)
		return "", err
	}

	fmt.Printf("Full LLM Response: %+v\n", result)

	// Extract from output[1].content[0].text
	if output, ok := result["output"].([]any); ok && len(output) > 1 {
		if secondOutput, ok := output[1].(map[string]any); ok {
			if content, ok := secondOutput["content"].([]any); ok && len(content) > 0 {
				if firstContent, ok := content[0].(map[string]any); ok {
					if text, ok := firstContent["text"].(string); ok {
						return cleanResponse(text), nil
					}
				}
			}
		}
	}

	return "", errors.New("could not read response from LLM")
}

func getOrCreateLLMRequest(q string) (string, error) {
	response, ok := getCache(q)
	if ok {
		return response, nil
	}

	// If this request is already in flight, wait for it to complete instead of creating a new request
	inFlightMutex.Lock()
	ch, ok := inFlightRequests[q]
	if ok {
		inFlightMutex.Unlock()
		// No matter if the request succeeded or not, the channel will be closed, letting us continue here
		// If it failed the cache will not be set, so we need to check ok to see if the request failed.
		<-ch
		response, ok := getCache(q)

		if !ok {
			return "", errors.New("upstream generation failed")
		}
		return response, nil
	}

	ch = make(chan bool)
	inFlightRequests[q] = ch
	inFlightMutex.Unlock()

	response, err := getLLMResponse(q)

	// If the request failed, return the error, for the server, close the channel so waiters can continue
	if err != nil {
		close(ch)
		return "", err
	}

	// Important to set the cache before removing the request from the inFlightRequests map
	// Otherwise, can have race condition where new request comes in before the cache is set,
	// and it will create a new LLM request.
	setCache(q, response)
	inFlightMutex.Lock()
	delete(inFlightRequests, q)
	inFlightMutex.Unlock()

	// Close the channel so waiters can continue
	close(ch)

	return response, nil
}

func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	h.handleDNSRequest(w, r)
}

func (h *dnsHandler) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		fmt.Println("No questions in request")
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	fmt.Println("Received DNS request for:", q.Name)

	if q.Qtype != dns.TypeTXT {
		fmt.Println("Unsupported DNS type:", q.Qtype)
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeNotImplemented
		w.WriteMsg(m)
		return
	}

	response, err := getOrCreateLLMRequest(q.Name)
	if err != nil {
		fmt.Println("Error getting LLM response:", err)
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Rcode = dns.RcodeSuccess

	reply := []dns.RR{}
	chunks := chunkString(response, 255)

	rr := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
		},
		Txt: chunks,
	}

	reply = append(reply, rr)

	m.Answer = reply
	w.WriteMsg(m)

}

func main() {
	var port = flag.Int("p", 53, "Port to listen on (default: 53)")
	flag.Parse()

	fmt.Printf("Starting DNS server on port %d\n", *port)

	err := dns.ListenAndServe(fmt.Sprintf(":%d", *port), "udp", &dnsHandler{})
	if err != nil {
		log.Fatalf("Failed to start DNS server: %v", err)
	}
}
