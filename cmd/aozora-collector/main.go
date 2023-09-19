package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/text/encoding/japanese"
)

type Entry struct {
	AuthorID string
	Author   string
	TitleID  string
	Title    string
	InfoURL  string
	ZipURL   string
}

var pageURLFormat = "https://www.aozora.gr.jp/cards/%s/card%s.html"

func findEntries(siteURL string) ([]Entry, error) {
	doc, err := goquery.NewDocument(siteURL)
	if err != nil {
		return nil, err
	}
	pat := regexp.MustCompile(`.*/cards/([0-9]+)/card([0-9]+).html$`)
	entries := []Entry{}
	doc.Find("ol li a").Each(func(n int, elem *goquery.Selection) {
		// 正規表現で[../cards/000879/card3819.html 000879 3819]という配列にして取得する
		token := pat.FindStringSubmatch(elem.AttrOr("href", ""))
		if len(token) != 3 {
			return
		}
		title := elem.Text()
		pageURL := fmt.Sprintf(pageURLFormat, token[1], token[2])
		author, zipURL := findAuthorAndZIP(pageURL)
		if zipURL != "" {
			entries = append(entries, Entry{
				AuthorID: token[1],
				Author:   author,
				TitleID:  token[2],
				Title:    title,
				InfoURL:  siteURL,
				ZipURL:   zipURL,
			})
		}
	})
	return entries, nil
}

func findAuthorAndZIP(siteURL string) (string, string) {
	doc, err := goquery.NewDocument(siteURL)
	if err != nil {
		return "", ""
	}

	author := doc.Find("table[summary=作家データ] tr:nth-child(2) td:nth-child(2)").First().Text()

	zipURL := ""
	doc.Find("table.download a").Each(func(n int, elem *goquery.Selection) {
		href := elem.AttrOr("href", "")
		if strings.HasSuffix(href, ".zip") {
			zipURL = href // ./files/4871_txt_20865.zipという相対パスになっている
		}
	})

	if zipURL == "" {
		return author, ""
	}
	if strings.HasPrefix(zipURL, "http://") || strings.HasPrefix(zipURL, "https://") {
		return author, zipURL
	}

	// zipファイルのURLを相対パスから絶対パスにする
	u, err := url.Parse(siteURL) // https://www.aozora.gr.jp/cards/000879/card25.html
	if err != nil {
		return author, ""
	}
	// u.Path : /cards/000879/card25.html
	// path.Dir(u.Path) : /cards/000879
	// path.Join(path.Dir(u.Path), zipURL) : https://www.aozora.gr.jp/cards/000879/files/3798_ruby_27269.zip
	u.Path = path.Join(path.Dir(u.Path), zipURL)
	return author, u.String()
}

func extractText(zipURL string) (string, error) {
	// zipファイルをダウンロードする
	resp, err := http.Get(zipURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// zipファイルの中身をbyte列で取得する
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// archive/zipパッケージで、zipファイルを読み込む
	r, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", err
	}

	// ファイル一覧からテキストファイルを抽出する
	for _, file := range r.File {
		if path.Ext(file.Name) == ".txt" {
			f, err := file.Open()
			if err != nil {
				return "", err
			}
			b, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				return "", err
			}

			// ShiftJISからUTF-8にエンコーディングする
			b, err = japanese.ShiftJIS.NewDecoder().Bytes(b)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}
	return "", errors.New("contents not found")
}

func setupDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	queries := []string{
		`CREATE TABLE IF NOT EXISTS authors(author_id TEXT, author TEXT, PRIMARY KEY (author_id))`,
		`CREATE TABLE IF NOT EXISTS contents(author_id TEXT, title_id TEXT, title TEXT, content TEXT, PRIMARY KEY (author_id, title_id))`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS contents_fts USING fts4(words)`,
	}
	for _, query := range queries {
		_, err = db.Exec(query)
		if err != nil {
			return nil, err
		}
	}
	return db, nil
}

func addEntry(db *sql.DB, entry *Entry, content string) error {
	_, err := db.Exec(`REPLACE INTO authors(author_id, author) values(?, ?)`, entry.AuthorID, entry.Author)
	if err != nil {
		return err
	}

	res, err := db.Exec(`REPLACE INTO contents(author_id, title_id, title, content) values(?, ?, ?, ?)`, entry.AuthorID, entry.TitleID, entry.Title, content)
	if err != nil {
		return err
	}
	docID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	t, err := tokenizer.New(ipa.Dict(), tokenizer.OmitBosEos())
	if err != nil {
		return err
	}

	seg := t.Wakati(content)
	_, err = db.Exec(`REPLACE INTO contents_fts(docid, words) values(?, ?)`, docID, strings.Join(seg, " "))
	if err != nil {
		return err
	}
	return nil
}

func main() {
	// DBを作成する
	db, err := setupDB("database.sqlite")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// 作家の書籍の一覧を取得する
	listURL := "https://www.aozora.gr.jp/index_pages/person879.html"
	entries, err := findEntries(listURL)
	if err != nil {
		log.Fatal(err)
	}

	// 書籍の内容を1件ずつ取得する
	for _, entry := range entries {
		content, err := extractText(entry.ZipURL)
		if err != nil {
			log.Println(err)
			continue
		}
		// DBに登録する
		err = addEntry(db, &entry, content)
		if err != nil {
			log.Println(err)
			continue
		}
	}
}
