// Copyright 2019 github.com/ucirello and https://cirello.io. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to writing, software distributed
// under the License is distributed on a "AS IS" BASIS, WITHOUT WARRANTIES OR
// CONDITIONS OF ANY KIND, either express or implied.
//
// See the License for the specific language governing permissions and
// limitations under the License.

// Command otp manages one-time passwords tokens, protecting them with a local
// private key (usually $HOME/.ssh/id_rsa) and storing its information in a
// encrypted db (usually at $HOME/.ssh/auth.db).
package main // import "cirello.io/otp"

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	otp "github.com/pquerna/otp/totp"
	"github.com/urfave/cli"
	_ "modernc.org/sqlite"
	"rsc.io/qr"
)

var homeDir string

func init() {
	log.SetPrefix("")
	log.SetFlags(0)

	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	homeDir = usr.HomeDir
}

func main() {
	app := cli.NewApp()
	app.Name = "OTP client"
	app.Usage = "command interface"
	app.Version = "1.0.0"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "db",
			Value:  filepath.Join(homeDir, ".ssh", "auth.db"),
			EnvVar: "OTP_DB",
		},
		cli.StringFlag{
			Name:   "private-key",
			Value:  filepath.Join(homeDir, ".ssh", "id_rsa"),
			EnvVar: "OTP_PRIVKEY",
		},
	}
	app.Commands = []cli.Command{
		initdb(),
		add(),
		get(),
		list(),
		genqr(),
		rm(),
		servehttp(),
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func initdb() cli.Command {
	return cli.Command{
		Name:  "init",
		Usage: "initialize the OTP database",
		Action: func(c *cli.Context) error {
			db, err := sql.Open("sqlite", c.GlobalString("db"))
			if err != nil {
				return err
			}
			defer db.Close()

			queries := []string{
				"CREATE TABLE IF NOT EXISTS `otps` (`id` INTEGER PRIMARY KEY, `account` char, `issuer` char, `password` blob);",
				"CREATE UNIQUE INDEX `otps_account_issuer` ON `otps`(`account`, `issuer`);",
			}

			for _, q := range queries {
				_, err = db.Exec(q)
				if err != nil {
					return err
				}
			}

			log.Println("database initialized")
			return nil
		},
	}
}

func add() cli.Command {
	return cli.Command{
		Name:      "add",
		Usage:     "a new OTP key",
		ArgsUsage: "`secret` `issuer` `account-name`",
		Action: func(c *cli.Context) error {
			priv, err := privkeyfile(c.GlobalString("private-key"))
			if err != nil {
				return err
			}

			secretkey := c.Args().Get(0)
			issuer := c.Args().Get(1)
			account := c.Args().Get(2)

			switch {
			case secretkey == "":
				return errors.New("secret key is missing")
			case issuer == "":
				return errors.New("issuer is missing")
			case account == "":
				return errors.New("account name is missing")
			}

			enckey, err := priv.encrypted([]byte(secretkey), cryptlabel(account, issuer))
			if err != nil {
				return err
			}

			db, err := sql.Open("sqlite", c.GlobalString("db"))
			if err != nil {
				return err
			}
			defer db.Close()

			_, err = db.Exec("REPLACE INTO `otps` (`issuer`, `account`, `password`) VALUES (?, ?, ?);", issuer, account, enckey)
			return err
		},
	}
}

func get() cli.Command {
	return cli.Command{
		Name:  "get",
		Usage: "generate OTP",
		Action: func(c *cli.Context) error {
			filter := c.Args().First()
			if filter == "" {
				return load(c, os.Stdout)
			}
			var buf bytes.Buffer
			if err := load(c, &buf); err != nil {
				return err
			}
			multiMatch := strings.Index(buf.String(), filter) != strings.LastIndex(buf.String(), filter)
			scanner := bufio.NewScanner(&buf)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, filter) {
					fields := strings.Fields(scanner.Text())
					if multiMatch {
						fmt.Println(line)
					} else {
						fmt.Println(fields[len(fields)-1])
					}
				}
			}
			return scanner.Err()
		},
	}
}

func servehttp() cli.Command {
	return cli.Command{
		Name:  "http",
		Usage: "serve OTP in a HTTP interface",
		Action: func(c *cli.Context) error {
			http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintln(w, "<html><body><pre>")
				load(c, w)
				fmt.Fprintln(w, "</pre></body></html>")
			})
			http.ListenAndServe(":9999", nil)
			return nil
		},
	}
}

func load(c *cli.Context, w io.Writer) error {
	priv, err := privkeyfile(c.GlobalString("private-key"))
	if err != nil {
		return err
	}

	db, err := sql.Open("sqlite", c.GlobalString("db"))
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query("SELECT `account`, `issuer`, `password` FROM `otps` ORDER BY `account` ASC, `issuer` ASC;")
	if err != nil {
		return err
	}
	defer rows.Close()

	tabw := tabwriter.NewWriter(w, 8, 8, 2, ' ', 0)
	defer tabw.Flush()
	fmt.Fprintln(tabw, "account\tissuer\texpiration\tcode")

	for rows.Next() {
		var (
			account, issuer string
			pw              []byte
		)
		rows.Scan(&account, &issuer, &pw)

		decrypted, err := priv.decrypted(pw, cryptlabel(account, issuer))
		if err != nil {
			return err
		}

		key := strings.ToUpper(strings.ReplaceAll(string(decrypted), " ", ""))
		token, err := otp.GenerateCode(key, time.Now())
		if err != nil {
			return err
		}

		line := fmt.Sprintf("%s\t%s\t%vs\t%s", account, issuer, (30 - time.Now().Unix()%30), token)
		fmt.Fprintln(tabw, line)
	}

	return nil
}

func list() cli.Command {
	return cli.Command{
		Name:  "list",
		Usage: "list all keys",
		Action: func(c *cli.Context) error {
			db, err := sql.Open("sqlite", c.GlobalString("db"))
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := db.Query("SELECT account, issuer FROM `otps` ORDER BY account ASC, issuer ASC;")
			if err != nil {
				return err
			}
			defer rows.Close()

			w := tabwriter.NewWriter(os.Stdout, 8, 8, 2, ' ', 0)
			defer w.Flush()
			fmt.Fprintln(w, "account\tissuer")

			for rows.Next() {
				var account, issuer string
				rows.Scan(&account, &issuer)
				fmt.Fprintln(w, fmt.Sprintf("%s\t%s", account, issuer))
			}

			return nil
		},
	}
}

func genqr() cli.Command {
	return cli.Command{
		Name:  "qr",
		Usage: "generate QR codes",
		Action: func(c *cli.Context) error {
			priv, err := privkeyfile(c.GlobalString("private-key"))
			if err != nil {
				return err
			}

			db, err := sql.Open("sqlite", c.GlobalString("db"))
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := db.Query("SELECT `account`, `issuer`, `password` FROM `otps` ORDER BY `account` ASC, `issuer` ASC;")
			if err != nil {
				return err
			}
			defer rows.Close()

			w := tabwriter.NewWriter(os.Stdout, 8, 8, 2, ' ', 0)
			defer w.Flush()
			fmt.Fprintln(w, "account\tissuer\tfile")

			for rows.Next() {
				var account, issuer string
				var pw []byte
				rows.Scan(&account, &issuer, &pw)

				decrypted, err := priv.decrypted(pw, cryptlabel(account, issuer))
				if err != nil {
					return err
				}

				qrfn, err := generateQR(issuer, account, string(decrypted))
				if err != nil {
					line := fmt.Sprintf("%s\t%s\t%s", account, issuer, err)
					fmt.Fprintln(w, line)
					continue
				}
				line := fmt.Sprintf("%s\t%s\t%s", account, issuer, qrfn)
				fmt.Fprintln(w, line)
			}

			return nil
		},
	}
}

func rm() cli.Command {
	return cli.Command{
		Name:      "rm",
		Usage:     "delete a OTP key",
		ArgsUsage: "`issuer` `account-name`",
		Action: func(c *cli.Context) error {
			issuer := c.Args().Get(0)
			account := c.Args().Get(1)

			switch {
			case issuer == "":
				return errors.New("issuer is missing")
			case account == "":
				return errors.New("account name is missing")
			}

			db, err := sql.Open("sqlite", c.GlobalString("db"))
			if err != nil {
				return err
			}
			defer db.Close()

			_, err = db.Exec("DELETE FROM `otps` WHERE `issuer` = ? AND `account` = ?;", issuer, account)
			return err
		},
	}
}

type privkey struct {
	*rsa.PrivateKey
}

func privkeyfile(fn string) (*privkey, error) {
	pemdata, err := os.ReadFile(fn)
	if err != nil {
		return nil, fmt.Errorf("cannot read key file: %s", err)
	}

	block, _ := pem.Decode(pemdata)
	if block == nil {
		return nil, errors.New("key data is not PEM encoded")
	}

	if got, want := block.Type, "RSA PRIVATE KEY"; got != want {
		return nil, fmt.Errorf("mismatched key type. got: %q want: %q", got, want)
	}

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %s", err)
	}

	return &privkey{PrivateKey: priv}, nil
}

func (p privkey) encrypted(in, label []byte) ([]byte, error) {
	return rsa.EncryptOAEP(sha256.New(), rand.Reader, &p.PublicKey, in, label)
}

func (p privkey) decrypted(in, label []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, p.PrivateKey, in, label)
}

func cryptlabel(account, issuer string) []byte {
	return []byte(fmt.Sprint(account, issuer))
}

func generateQR(issuer, account, password string) (string, error) {
	otpauth := fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s", issuer, account, password, issuer)
	code, err := qr.Encode(otpauth, qr.H)
	if err != nil {
		return "", err
	}

	img, _, err := image.Decode(bytes.NewReader(code.PNG()))
	if err != nil {
		panic(err)
	}

	fn := fmt.Sprintf("otp-qr-%s-%s.png", issuer, account)
	out, err := os.Create(fn)
	if err != nil {
		return "", err
	}

	err = png.Encode(out, img)
	if err != nil {
		return "", err
	}

	return fn, nil
}
