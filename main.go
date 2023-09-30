package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"rsc.io/pdf"

	_ "github.com/lib/pq"
	g "github.com/serpapi/google-search-results-golang"
)

const CONFIG_FILE = "config.json"
const B_LIMIT = 1048575

var db *sql.DB
var errDb error
var pattern string
var tgrmResults []TgrmResult

type ResultBody struct {
	Title string
	Link  string
}

type ViewData struct {
	Results []TgrmResult
	Message string
}

type Config struct {
	Catalog    string
	ConnString string
}

type TgrmResult struct {
	Link     string
	Filename string
	Score    float64
	Founds   int64
}

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/get_request", func(w http.ResponseWriter, r *http.Request) {
		request := r.FormValue("request_txt")
		var start int
		var results []interface{}
		for true {
			parameter := map[string]string{
				"q":     request + " filetype:pdf",
				"start": strconv.Itoa(start),
				"num":   "5",
			}
			api := "af1a018010e31b1b87160b896e5fb00a38ee6009788bed781ccf2313b45f01b0"
			query := g.NewGoogleSearch(parameter, api)
			response, err := query.GetJSON()
			if err != nil {
				fmt.Fprintf(w, "Возникла ошибка при поиске: %s\n", err.Error())
				return
			}
			tmp := response["organic_results"].([]interface{})
			results = append(results, tmp...)
			if len(tmp) < 5 {
				break
			} else {
				start += 5
			}
		}
		config, errRead := ioutil.ReadFile(CONFIG_FILE)
		if errRead != nil {
			fmt.Fprintf(w, "Ошибка при чтении конфигурации: %s\n", errRead.Error())
			return
		}
		var conf Config
		if errJson := json.Unmarshal(config, &conf); errJson != nil {
			fmt.Fprintf(w, "Ошибка при чтении конфигурации: %s\n", errJson.Error())
			return
		}
		resultsCount := len(results)
		downloaded := 0
		errDb = TryOpenDb(conf.ConnString)
		if errDb != nil {
			fmt.Fprintf(w, "Ошибка при подключении к БД: %s\n", errDb.Error())
			return
		}
		defer db.Close()
		var message string
		for i := 0; i < resultsCount; i++ {
			current := results[i].(map[string]interface{})
			fileName := current["title"].(string)
			if !strings.HasSuffix(fileName, ".pdf") {
				fileName += ".pdf"
			}
			filePath := conf.Catalog + "/" + fileName
			link := current["link"].(string)
			err := DownloadFile(filePath, link)
			if err != nil {
				message += fmt.Sprintf("Ошибка при скачивании файла %s: %s\n",
					fileName, err.Error())
			} else {
				downloaded++
				dbQuery := "INSERT INTO results (title, url) VALUES ($1, $2)"
				_, errExec := db.Exec(dbQuery, fileName, link)
				if errExec != nil {
					fmt.Fprintf(w, "Ошибка при записи в БД: %s\n", errExec.Error())
				}
			}
		}
		message += fmt.Sprintf("Итого скачано: %d\n", downloaded)
		tmpl, _ := template.ParseFiles("templates/get_request.html")
		tmpl.Execute(w, message)
	})
	http.HandleFunc("/tgrm", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "templates/tgrm.html")
	})
	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		pattern = r.FormValue("request_txt")
		config, errRead := ioutil.ReadFile(CONFIG_FILE)
		if errRead != nil {
			fmt.Fprintf(w, "Ошибка при чтении конфигурации: %s\n", errRead.Error())
			return
		}
		var conf Config
		var message string
		if errJson := json.Unmarshal(config, &conf); errJson != nil {
			fmt.Fprintf(w, "Ошибка при чтении конфигурации: %s\n", errJson.Error())
			return
		}
		errDb = TryOpenDb(conf.ConnString)
		if errDb != nil {
			fmt.Fprintf(w, "Ошибка при подключении к БД: %s\n", errDb.Error())
			return
		}
		defer db.Close()
		err := filepath.Walk(conf.Catalog, walkFunc)
		if err != nil {
			message += fmt.Sprintf("Ошибка при обходе файлов: %s\n", err.Error())
		}
		tmpl, _ := template.ParseFiles("templates/search.html")
		data := ViewData{tgrmResults, message}
		tmpl.Execute(w, data)
	})
	http.ListenAndServe(":83", nil)
}

func TryOpenDb(connString string) error {
	db, errDb = sql.Open("postgres", connString)
	if errDb != nil {
		return errDb
	}
	return nil
}

func ExecuteDbQuery(dbQuery string, errMessage string, args ...any) error {
	_, errExec := db.Exec(dbQuery, args...)
	if errExec != nil {
		return errors.New(errMessage + errExec.Error())
	}
	return nil
}

func walkFunc(path string, info os.FileInfo, err error) (errFunc error) {
	defer func() {
		if rec := recover(); rec != nil {
			errFunc = errors.New(info.Name() + ": " + fmt.Sprint(rec))
		}
	}()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	if !strings.HasSuffix(path, ".pdf") {
		return nil
	}
	file, errRead := pdf.Open(path)
	if errRead != nil {
		return errors.New(info.Name() + ": " + errRead.Error())
	}
	pages := file.NumPage()
	fileContent := ""
	for i := 1; i <= pages; i++ {
		text := file.Page(i).Content().Text
		for j := 0; j < len(text); j++ {
			fileContent += text[j].S
		}
	}
	var runes []rune
	for _, r := range fileContent {
		if r != 0 {
			runes = append(runes, r)
		}
	}
	fileContent = string(runes)
	greater := len(fileContent) / B_LIMIT
	var chunks []string
	var idx1, idx2 int
	var dbQuery string
	var errExec error
	if greater == 0 {
		chunks = append(chunks, fileContent)
	} else {
		for ch := 0; ch <= greater; ch++ {
			idx1 = idx2
			idx2 = len(fileContent) / (greater + 1) * (ch + 1)
			if ch == greater {
				idx2 = len(fileContent)
			}
			chunks = append(chunks, fileContent[idx1:idx2])
		}
	}
	for ch := 0; ch <= greater; ch++ {
		dbQuery = "INSERT INTO documents (body) VALUES ($1)"
		errExec = ExecuteDbQuery(dbQuery, "Ошибка при записи в БД: ", chunks[ch])
		if errExec != nil {
			return errExec
		}
	}
	for ch := 0; ch <= greater; ch++ {
		if ch == 0 {
			dbQuery = `CREATE TABLE words AS SELECT word FROM 
				ts_stat('SELECT to_tsvector(body) FROM documents
					WHERE id = (SELECT min(id) FROM documents)')`
		} else {
			idxStr := strconv.Itoa(ch)
			dbQuery = `INSERT INTO words SELECT word FROM
				ts_stat('SELECT to_tsvector(body) FROM documents
					WHERE id = (SELECT min(id)+` + idxStr + ` FROM documents)')`
		}
		errExec = ExecuteDbQuery(dbQuery,
			"Ошибка при составлении таблицы триграммного поиска: ")
		if errExec != nil {
			return errExec
		}
	}
	dbQuery = `CREATE INDEX words_idx ON words USING GIST (word gist_trgm_ops)`
	errExec = ExecuteDbQuery(dbQuery, "Ошибка при создании индекса: ")
	if errExec != nil {
		return errExec
	}
	defer func() {
		errFunc = ClearDB()
	}()
	dbQuery = `SELECT word, similarity(word, $1) AS sml FROM words
		WHERE word % $1 ORDER BY sml DESC`
	rows, errRead := db.Query(dbQuery, pattern)
	if errRead != nil {
		return errors.New("Ошибка при триграммном поиске: " + errRead.Error())
	}
	defer rows.Close()
	tgrmResult := TgrmResult{strings.ReplaceAll(path, `\`, "/"),
		info.Name(), 0, 0}
	var k int64
	for rows.Next() {
		var tmp float64
		var word string
		errExec = rows.Scan(&word, &tmp)
		if errExec != nil {
			return errors.New("Ошибка при чтении результатов: " + errExec.Error())
		}
		tgrmResult.Score += tmp
		k++
		if tmp == 1 {
			tgrmResult.Founds++
		}
	}
	if k == 0 {
		tgrmResult.Score = 0
	} else {
		tgrmResult.Score /= float64(k)
	}
	tgrmResults = append(tgrmResults, tgrmResult)
	return nil
}

func ClearDB() error {
	dbQuery := "DROP INDEX IF EXISTS words_idx"
	errExec := ExecuteDbQuery(dbQuery, "Ошибка при удалении индекса: ")
	if errExec != nil {
		return errExec
	}
	dbQuery = "DROP TABLE IF EXISTS words"
	errExec = ExecuteDbQuery(dbQuery, "Ошибка при удалении таблицы: ")
	if errExec != nil {
		return errExec
	}
	dbQuery = "DELETE FROM documents"
	errExec = ExecuteDbQuery(dbQuery, "Ошибка при удалении записей: ")
	if errExec != nil {
		return errExec
	}
	return nil
}

func DownloadFile(filepath string, url string) error {

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}
