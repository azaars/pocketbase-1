package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/ghupdate"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/hook"
)

func main() {
	app := pocketbase.New()

	// ---------------------------------------------------------------
	// Optional plugin flags:
	// ---------------------------------------------------------------

	var hooksDir string
	app.RootCmd.PersistentFlags().StringVar(
		&hooksDir,
		"hooksDir",
		"",
		"the directory with the JS app hooks",
	)

	var hooksWatch bool
	app.RootCmd.PersistentFlags().BoolVar(
		&hooksWatch,
		"hooksWatch",
		true,
		"auto restart the app on pb_hooks file change",
	)

	var hooksPool int
	app.RootCmd.PersistentFlags().IntVar(
		&hooksPool,
		"hooksPool",
		15,
		"the total prewarm goja.Runtime instances for the JS app hooks execution",
	)

	var migrationsDir string
	app.RootCmd.PersistentFlags().StringVar(
		&migrationsDir,
		"migrationsDir",
		"",
		"the directory with the user defined migrations",
	)

	var automigrate bool
	app.RootCmd.PersistentFlags().BoolVar(
		&automigrate,
		"automigrate",
		true,
		"enable/disable auto migrations",
	)

	var publicDir string
	app.RootCmd.PersistentFlags().StringVar(
		&publicDir,
		"publicDir",
		defaultPublicDir(),
		"the directory to serve static files",
	)

	var indexFallback bool
	app.RootCmd.PersistentFlags().BoolVar(
		&indexFallback,
		"indexFallback",
		true,
		"fallback the request to index.html on missing static path (eg. when pretty urls are used with SPA)",
	)

	app.RootCmd.ParseFlags(os.Args[1:])

	// ---------------------------------------------------------------
	// Plugins and hooks:
	// ---------------------------------------------------------------

	// load jsvm (pb_hooks and pb_migrations)
	jsvm.MustRegister(app, jsvm.Config{
		MigrationsDir: migrationsDir,
		HooksDir:      hooksDir,
		HooksWatch:    hooksWatch,
		HooksPoolSize: hooksPool,
	})

	// migrate command (with js templates)
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		TemplateLang: migratecmd.TemplateLangJS,
		Automigrate:  automigrate,
		Dir:          migrationsDir,
	})

	// GitHub selfupdate
	ghupdate.MustRegister(app, app.RootCmd, ghupdate.Config{})

	// static route to serves files from the provided public dir
	// (if publicDir exists and the route path is not already defined)
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Func: func(e *core.ServeEvent) error {
			if !e.Router.HasRoute(http.MethodGet, "/{path...}") {
				e.Router.GET("/{path...}", apis.Static(os.DirFS(publicDir), indexFallback))
			}

			return e.Next()
		},
		Priority: 999, // execute as latest as possible to allow users to provide their own route
	})

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.POST("/api/collections/users/send-tac", func(re *core.RequestEvent) error {
			data := struct {
				Phone    string `json:"phone" form:"phone"`
				Language string `json:"lang" form:"lang"`
			}{}
			if err := re.BindBody(&data); err != nil {
				return apis.NewBadRequestError("Failed to read request data", err)
			}
			record, err := app.FindFirstRecordByData("users", "phone", data.Phone)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return apis.NewBadRequestError("Invalid phone number", err)
			}

			payload := map[string]interface{}{
				"msisdn": data.Phone,
				"lang":   data.Language,
			}
			if record == nil {
				payload["id"] = nil
			} else {
				payload["id"] = record.Id
			}
			response, httpCode, err := sendTAC(payload)
			if err != nil {
				return apis.NewApiError(500, err.Error(), err)
			}

			if httpCode == 404 {
				if record != nil { // no longer a subscriber
					app.DB().Update("users", dbx.Params{"status": "Terminated"}, dbx.HashExp{"id": record.Id}).Execute()
					app.DB().Update("dealers", dbx.Params{"status": "Terminated"}, dbx.HashExp{"userId": record.Id}).Execute()
				}
				return apis.NewBadRequestError("Invalid phone number", err)
			} else if httpCode == 200 {
				if record == nil {
					record, err = app.FindFirstRecordByData("users", "phone", data.Phone)
					if err != nil {
						return apis.NewInternalServerError("Faild to create user: "+err.Error(), err)
					}
				}
				_, err = app.DB().Update("users", dbx.Params{"tac": response["tac"]}, dbx.HashExp{"id": record.Id}).Execute()
				if err != nil {
					return apis.NewApiError(500, "Failed to create TAC: "+err.Error(), err)
				}
				if dealer, exists := response["dealer"]; exists {
					var user struct {
						ID, UserId string
					}
					err = app.DB().Select("id, userId").From("dealers").Where(dbx.HashExp{"dealer": dealer}).One(&user)
					if err == nil || user.UserId != record.Id {
						if err != nil {
							app.DB().Delete("dealers", dbx.HashExp{"userId": user.ID}).Execute()
						}
						params := dbx.Params{
							"userId": record.Id,
							"dealer": dealer,
						}
						_, err = app.DB().Insert("dealers", params).Execute()
						if err != nil {
							return apis.NewApiError(500, "Failed to create dealer: "+err.Error(), err)
						}
					}
				} else {
					app.DB().Delete("dealers", dbx.HashExp{"userId": record.Id}).Execute()
				}
			} else {
				if message, exists := response["message"]; exists {
					err := errors.New(fmt.Sprint(message))
					return apis.NewApiError(500, err.Error(), err)
				} else {
					return apis.NewApiError(500, "Failed to send TAC:", err)
				}
			}
			return nil
		})

		return se.Next()
	})

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.POST("/api/collections/users/phone-login", func(re *core.RequestEvent) error {
			data := struct {
				Phone string `json:"phone" form:"phone"`
				TAC   string `json:"tac" form:"tac"`
			}{}
			if err := re.BindBody(&data); err != nil {
				return apis.NewBadRequestError("Failed to read request data", err)
			}
			record, err := app.FindFirstRecordByData("users", "phone", data.Phone)
			if err != nil || strings.Compare(data.TAC, record.GetString("tac")) != 0 {
				return apis.NewBadRequestError("Invalid credentials", err)
			}

			return apis.RecordAuthResponse(re, record, "", nil)
		})

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// the default pb_public dir location is relative to the executable
func defaultPublicDir() string {
	if strings.HasPrefix(os.Args[0], os.TempDir()) {
		// most likely ran with go run
		return "./pb_public"
	}

	return filepath.Join(os.Args[0], "../pb_public")
}

// postAPI makes a POST request to the specified URL with a JSON payload and returns the response body as a string.
func sendTAC(payload map[string]interface{}) (map[string]interface{}, int, error) {
	url := "https://rest.onexox.my/sendTAC"

	// Marshal the payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, 500, fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Create a new HTTP client
	client := &http.Client{}

	// Create a new HTTP POST request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, 500, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers for the request
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Go-Client")

	// Perform the HTTP request
	resp, err := client.Do(req)
	if err != nil {
		return nil, 500, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 500, fmt.Errorf("failed to read response body: %w", err)
	}

	// Unmarshal the response JSON into a map
	var responseMap map[string]interface{}
	err = json.Unmarshal(body, &responseMap)
	if err != nil {
		return nil, 500, fmt.Errorf("failed to unmarshal response JSON: %w", err)
	}

	return responseMap, resp.StatusCode, nil
}
