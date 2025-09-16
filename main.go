package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/playwright-community/playwright-go"
)

type ConfigSettings struct {
	Timeout    int      `json:"timeout"`
	MaxWorkers int      `json:"max_workers"`
	MinDelay   int      `json:"min_delay"`
	MaxDelay   int      `json:"max_delay"`
	Headless   bool     `json:"headless"`
	Proxies    []string `json:"proxies"`
	MaxRetries int      `json:"max_retries"`
}

type InputData struct {
	Cities []string `json:"cities"`
	AdIDs  []int    `json:"ad_ids"`
	Query  string   `json:"query"`
}

type City struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type PositionResult struct {
	City      string         `json:"city"`
	Positions map[int]string `json:"positions"`
	Error     string         `json:"error,omitempty"`
	ProxyUsed string         `json:"proxy_used,omitempty"`
	Blocked   bool           `json:"blocked,omitempty"`
}

var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
}

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))
	log.SetOutput(os.Stderr)

	config, err := loadConfig("config.json")
	if err != nil {
		log.Printf("Ошибка загрузки config.json: %v. Используются значения по умолчанию.", err)
		config = getDefaultConfig()
	}

	inputData, err := loadInputData()
	if err != nil {
		log.Fatalf("Ошибка загрузки данных из stdin: %v", err)
	}

	cities := make([]City, len(inputData.Cities))
	for i, citySlug := range inputData.Cities {
		cities[i] = City{
			Name: citySlug,
			Slug: citySlug,
		}
	}

	pw, err := playwright.Run()
	if err != nil {
		log.Fatalf("Не удалось запустить Playwright: %v", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(config.Headless),
	})
	if err != nil {
		log.Fatalf("Не удалось запустить браузер: %v", err)
	}
	defer browser.Close()

	cityGroups := distributeCities(cities, config.Proxies)
	resultsChan := make(chan PositionResult, len(cities))
	var wg sync.WaitGroup

	for proxy, cities := range cityGroups {
		wg.Add(1)
		go func(proxy string, cities []City) {
			defer wg.Done()
			processCities(browser, cities, proxy, config, inputData, resultsChan)
		}(proxy, cities)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var allResults []PositionResult
	for result := range resultsChan {
		allResults = append(allResults, result)
	}

	jsonData, err := json.MarshalIndent(allResults, "", "  ")
	if err != nil {
		log.Fatalf("Ошибка маршалинга результатов: %v", err)
	}
	fmt.Println(string(jsonData))
}

func loadConfig(filename string) (ConfigSettings, error) {
	var config ConfigSettings
	file, err := os.Open(filename)
	if err != nil {
		return config, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	return config, err
}

func getDefaultConfig() ConfigSettings {
	return ConfigSettings{
		Timeout:    30,
		MaxWorkers: 3,
		MinDelay:   3,
		MaxDelay:   10,
		Headless:   true,
		Proxies:    []string{},
		MaxRetries: 2,
	}
}

func loadInputData() (InputData, error) {
	var inputData InputData
	decoder := json.NewDecoder(os.Stdin)
	err := decoder.Decode(&inputData)
	return inputData, err
}

func distributeCities(cities []City, proxies []string) map[string][]City {
	groups := make(map[string][]City)
	groups[""] = []City{}

	for _, proxy := range proxies {
		groups[proxy] = []City{}
	}

	for i, city := range cities {
		if len(proxies) > 0 {
			proxyIndex := i % len(proxies)
			proxy := proxies[proxyIndex]
			groups[proxy] = append(groups[proxy], city)
		} else {
			groups[""] = append(groups[""], city)
		}
	}

	return groups
}

func processCities(browser playwright.Browser, cities []City, proxy string, config ConfigSettings, inputData InputData, resultsChan chan<- PositionResult) {
	if len(cities) == 0 {
		return
	}

	var context playwright.BrowserContext
	var err error

	if proxy != "" {
		log.Printf("Создаем контекст с прокси: %s", proxy)

		u, err := url.Parse(proxy)
		if err != nil {
			log.Printf("Ошибка парсинга прокси %s: %v. Работаем без прокси.", proxy, err)
			context, err = browser.NewContext()
			proxy = ""
		} else {
			pwProxy := playwright.Proxy{
				Server: u.Scheme + "://" + u.Host,
			}
			if u.User != nil {
				username := u.User.Username()
				pwProxy.Username = &username

				if pwd, ok := u.User.Password(); ok {
					pwProxy.Password = &pwd
				}
			}

			context, err = browser.NewContext(playwright.BrowserNewContextOptions{
				Proxy: &pwProxy,
			})
		}

	} else {
		context, err = browser.NewContext()
	}

	if err != nil {
		log.Printf("Не удалось создать контекст браузера: %v", err)
		for _, city := range cities {
			resultsChan <- PositionResult{
				City:      city.Name,
				Positions: make(map[int]string),
				Error:     fmt.Sprintf("Не удалось создать контекст браузера: %v", err),
				ProxyUsed: proxy,
			}
		}
		return
	}
	defer context.Close()

	initScript := `
		Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		Object.defineProperty(navigator, 'plugins', { get: () => [1, 2, 3, 4, 5] });
		Object.defineProperty(navigator, 'languages', { get: () => ['ru-RU', 'ru', 'en'] });
		window.chrome = { runtime: {} };
	`
	err = context.AddInitScript(playwright.Script{
		Content: &initScript,
	})
	if err != nil {
		log.Printf("Не удалось добавить init script: %v", err)
	}

	for _, city := range cities {
		delay := time.Duration(rand.Intn(config.MaxDelay-config.MinDelay+1)+config.MinDelay) * time.Second
		log.Printf("Ожидание %v перед запросом для города %s (прокси: %s)", delay, city.Name, proxy)
		time.Sleep(delay)

		result := parseCityWithRetry(context, city, inputData.Query, inputData.AdIDs, config)
		result.ProxyUsed = proxy
		resultsChan <- result
	}
}

func parseCityWithRetry(context playwright.BrowserContext, city City, query string, adIDs []int, config ConfigSettings) PositionResult {
	var result PositionResult

	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		if attempt > 0 {
			retryDelay := time.Duration(attempt*5) * time.Second
			log.Printf("Повторная попытка %d для города %s через %v", attempt, city.Name, retryDelay)
			time.Sleep(retryDelay)
		}

		result = parseCity(context, city, query, adIDs, config)

		if result.Error == "" || (!result.Blocked && !strings.Contains(result.Error, "timeout")) {
			break
		}

		log.Printf("Попытка %d для города %s не удалась: %s", attempt, city.Name, result.Error)
	}

	return result
}

func parseCity(context playwright.BrowserContext, city City, query string, adIDs []int, config ConfigSettings) PositionResult {
	result := PositionResult{
		City:      city.Name,
		Positions: make(map[int]string),
	}

	page, err := context.NewPage()
	if err != nil {
		result.Error = fmt.Sprintf("Не удалось создать страницу: %v", err)
		return result
	}
	defer page.Close()

	randomUserAgent := userAgents[rand.Intn(len(userAgents))]
	err = page.SetExtraHTTPHeaders(map[string]string{
		"User-Agent":      randomUserAgent,
		"Accept-Language": "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7",
	})
	if err != nil {
		log.Printf("Не удалось установить заголовки: %v", err)
	}

	encodedQuery := strings.ReplaceAll(query, " ", "+")
	avitoURL := fmt.Sprintf("https://www.avito.ru/%s?q=%s", city.Slug, encodedQuery)

	timeout := float64(config.Timeout) * 1000
	if !config.Headless {
		timeout = 15000
	}

	_, err = page.Goto(avitoURL, playwright.PageGotoOptions{
		Timeout:   playwright.Float(timeout),
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	})

	if err != nil {
		currentURL := page.URL()
		if currentURL != "" && currentURL != "about:blank" {
			result.Error = "Страница загружена частично, но произошел таймаут"
		} else {
			result.Error = fmt.Sprintf("Не удалось перейти на страницу: %v", err)
		}

		if strings.Contains(err.Error(), "timeout") {
			result.Blocked = true
		}
		return result
	}

	time.Sleep(2 * time.Second)

	// if isBlocked, reason := checkIfBlocked(page); isBlocked {
	// 	result.Error = fmt.Sprintf("Обнаружена блокировка: %s", reason)
	// 	result.Blocked = true поху тестим
	// 	return result
	// }

	_, err = page.WaitForSelector("[data-item-id]", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(10000),
	})

	if err != nil {
		result.Error = "Не удалось дождаться появления объявлений"
		return result
	}

	for i := 0; i < 3; i++ {
		scrollResult, err := page.Evaluate(`() => {
			if (document.body && document.body.scrollHeight) {
				window.scrollBy(0, document.body.scrollHeight / 3);
				return {success: true, height: document.body.scrollHeight};
			}
			return {success: false, height: 0};
		}`)

		if err != nil {
			log.Printf("Ошибка при скролле: %v", err)
		} else if scrollResult != nil {
			if scrollMap, ok := scrollResult.(map[string]interface{}); ok {
				if success, ok := scrollMap["success"].(bool); ok && success {
					log.Printf("Успешно прокрутили на %vpx", scrollMap["height"])
				}
			}
		}
		time.Sleep(1 * time.Second)
	}

	items, err := page.QuerySelectorAll("[data-item-id]")
	if err != nil {
		result.Error = fmt.Sprintf("Не удалось найти объявления: %v", err)
		return result
	}
	if len(items) == 0 {
		result.Error = "На странице не найдено объявлений"
		return result
	}

	for pos, item := range items {
		if pos >= 50 {
			break
		}

		itemIDStr, err := item.GetAttribute("data-item-id")
		if err != nil {
			continue
		}

		itemID, err := strconv.Atoi(itemIDStr)
		if err != nil {
			continue
		}

		for _, targetID := range adIDs {
			if itemID == targetID {
				result.Positions[targetID] = strconv.Itoa(pos + 1)
				break
			}
		}
	}

	for _, targetID := range adIDs {
		if _, found := result.Positions[targetID]; !found {
			result.Positions[targetID] = "50+"
		}
	}

	return result
}

func checkIfBlocked(page playwright.Page) (bool, string) {
	title, err := page.Title()
	if err == nil {
		if strings.Contains(strings.ToLower(title), "captcha") ||
			strings.Contains(strings.ToLower(title), "recaptcha") ||
			strings.Contains(title, "Доступ ограничен") {
			return true, "page_title: " + title
		}
	}

	// Проверяем основные селекторы блокировок
	blockSelectors := map[string]string{
		"captcha":     ".captcha, [data-captcha]",
		"recaptcha":   ".g-recaptcha, iframe[src*='recaptcha']",
		"cloudflare":  "#challenge-error-title, .cf-error-title",
		"avito_block": "[data-marker*='captcha'], [data-marker*='block']",
		"ip_block":    "//*[contains(text(), 'Проблема с IP') or contains(text(), 'IP address')]",
	}

	for reason, selector := range blockSelectors {
		visible, err := page.IsVisible(selector)
		if err == nil && visible {
			return true, "selector: " + reason
		}
	}

	content, err := page.TextContent("body")
	if err == nil {
		contentLower := strings.ToLower(content)
		blockIndicators := []string{
			"recaptcha",
			"captcha",
			"cloudflare",
			"подтвердите что вы не робот",
			"проблема с ip",
			"доступ ограничен",

			"ваш ip адрес",
		}

		for _, indicator := range blockIndicators {
			if strings.Contains(contentLower, indicator) {
				return true, "text_content: " + indicator
			}
		}
	}

	iframes, err := page.QuerySelectorAll("iframe")
	if err == nil {
		for _, iframe := range iframes {
			src, err := iframe.GetAttribute("src")
			if err == nil && src != "" {
				if strings.Contains(src, "recaptcha") || strings.Contains(src, "captcha") {
					return true, "iframe_src: " + src
				}
			}
		}
	}

	return false, ""
}
