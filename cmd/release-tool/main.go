/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"
	"unicode"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

type note struct {
	Title       string `toml:"title"`
	Description string `toml:"description"`
}

type change struct {
	Commit      string `toml:"commit"`
	Description string `toml:"description"`
}

type dependency struct {
	Name     string
	Ref      string
	Sha      string
	Previous string
	GitURL   string
}

type download struct {
	Filename string
	Hash     string
}

type projectChange struct {
	Name    string
	Since   string
	Changes []change
}

type projectRename struct {
	Old string `toml:"old"`
	New string `toml:"new"`
}

type makeDependency struct {
	Variable   string `toml:"variable"`
	Repository string `toml:"repository"`
}

type release struct { //nolint: govet
	ProjectName     string            `toml:"project_name"`
	GithubRepo      string            `toml:"github_repo"`
	Commit          string            `toml:"commit"`
	Previous        string            `toml:"previous"`
	PreRelease      bool              `toml:"pre_release"`
	Preface         string            `toml:"preface"`
	Notes           map[string]note   `toml:"notes"`
	BreakingChanges map[string]change `toml:"breaking"`
	ReleaseDate     string            `toml:"release_date"`

	// dependency options
	MatchDeps  string                    `toml:"match_deps"`
	RenameDeps map[string]projectRename  `toml:"rename_deps"`
	IgnoreDeps []string                  `toml:"ignore_deps"`
	MakeDeps   map[string]makeDependency `toml:"make_deps"`

	// generated fields
	Changes      []projectChange
	Contributors []string
	Dependencies []dependency
	Tag          string
	Version      string
	Downloads    []download
}

//nolint:gocognit,gocyclo,cyclop,maintidx
func main() {
	app := cli.NewApp()
	app.Name = "release"
	app.Description = `release tooling.

This tool should be ran from the root of the project repository for a new release.
`
	app.Flags = []cli.Flag{
		&cli.BoolFlag{
			Name:    "dry",
			Aliases: []string{"n"},
			Usage:   "run the release tooling as a dry run to print the release notes to stdout",
		},
		&cli.BoolFlag{
			Name:    "debug",
			Aliases: []string{"d"},
			Usage:   "show debug output",
		},
		&cli.StringFlag{
			Name:    "tag",
			Aliases: []string{"t"},
			Usage:   "tag name for the release, defaults to release file name",
		},
		&cli.StringFlag{
			Name:  "template",
			Usage: "template filepath to use in place of the default",
			Value: defaultTemplateFile,
		},
		&cli.BoolFlag{
			Name:    "linkify",
			Aliases: []string{"l"},
			Usage:   "add links to changelog",
		},
		&cli.BoolFlag{
			Name:    "gfm",
			Aliases: []string{"g"},
			Usage:   "use GitHub Flavored Markdown links",
		},
		&cli.StringFlag{
			Name:    "cache",
			Usage:   "cache directory for static remote resources",
			EnvVars: []string{"RELEASE_TOOL_CACHE"},
		},
	}
	app.Action = func(context *cli.Context) error {
		var (
			releasePath = context.Args().First()
			tag         = context.String("tag")
			linkify     = context.Bool("linkify")
			gfm         = context.Bool("gfm")
		)

		if tag == "" {
			tag = parseTag(releasePath)
		}

		version := strings.TrimLeft(tag, "v")

		if context.Bool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}

		var (
			cache   Cache
			gitRoot string
		)

		cd := context.String("cache")
		if cd == "" {
			cache = nilCache{}
		} else {
			var err error

			cd, err = filepath.Abs(cd)
			if err != nil {
				return err
			}

			if _, err = os.Stat(cd); err != nil {
				return fmt.Errorf("unable to use cache dir: %w", err)
			}

			gitRoot = filepath.Join(cd, "git")
			cacheRoot := filepath.Join(cd, "object")

			if err = os.MkdirAll(gitRoot, 0o755); err != nil {
				return fmt.Errorf("unable to mkdir %s: %w", gitRoot, err)
			}

			if err = os.MkdirAll(cacheRoot, 0o755); err != nil {
				return fmt.Errorf("unable to mkdir: %s: %w", cacheRoot, err)
			}

			cache = &dirCache{
				root: cacheRoot,
			}
		}

		r, err := loadRelease(releasePath)
		if err != nil {
			return err
		}

		logrus.Infof("Welcome to the %s release tool...", r.ProjectName)

		mailmapPath, err := filepath.Abs(".mailmap")
		if err != nil {
			return fmt.Errorf("failed to resolve mailmap: %w", err)
		}

		gitConfigs["mailmap.file"] = mailmapPath

		var (
			contributors   = map[contributor]int{}
			projectChanges = []projectChange{}
		)

		changes, err := changelog(r.Previous, r.Commit)
		if err != nil {
			return err
		}

		if linkify {
			if err = linkifyChanges(changes, githubCommitLink(r.GithubRepo, gfm), githubPRLink(r.GithubRepo), gfm); err != nil {
				return err
			}
		}

		projectChanges = append(projectChanges, projectChange{
			Name:    "",
			Changes: changes,
		})

		previousTag, err := getPreviousTag(tag)
		if err != nil {
			return err
		}

		if previousTag != "" && previousTag != r.Previous && previousTag != r.Tag {
			var previousTagChanges []change

			previousTagChanges, err = changelog(previousTag, r.Commit)
			if err != nil {
				return err
			}

			if linkify {
				if err = linkifyChanges(previousTagChanges, githubCommitLink(r.GithubRepo, gfm), githubPRLink(r.GithubRepo), gfm); err != nil {
					return err
				}
			}

			projectChanges = append(projectChanges, projectChange{
				Name:    "",
				Since:   previousTag,
				Changes: previousTagChanges,
			})
		}

		if err = addContributors(r.Previous, r.Commit, contributors); err != nil {
			return err
		}

		logrus.Infof("creating new release %s with %d new changes...", tag, len(changes))

		makeDeps := make([]makeDependency, 0, len(r.MakeDeps))

		for _, makeDep := range r.MakeDeps {
			makeDeps = append(makeDeps, makeDep)
		}

		current, err := parseDependencies(r.Commit, makeDeps)
		if err != nil {
			return err
		}

		previous, err := parseDependencies(r.Previous, makeDeps)
		if err != nil {
			return err
		}

		renameDependencies(previous, r.RenameDeps)

		updatedDeps, err := getUpdatedDeps(previous, current, r.IgnoreDeps, cache)
		if err != nil {
			return err
		}

		sort.Slice(updatedDeps, func(i, j int) bool {
			return updatedDeps[i].Name < updatedDeps[j].Name
		})

		if r.MatchDeps != "" && len(updatedDeps) > 0 {
			var re *regexp.Regexp

			re, err = regexp.Compile(r.MatchDeps)
			if err != nil {
				return fmt.Errorf("unable to compile 'match_deps' regexp: %w", err)
			}

			if gitRoot == "" {
				var td string

				td, err = os.MkdirTemp("", "tmp-clone-")
				if err != nil {
					return fmt.Errorf("unable to create temp clone directory: %w", err)
				}

				defer os.RemoveAll(td) //nolint: errcheck
				gitRoot = td
			}

			var cwd string

			cwd, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("unable to get cwd: %w", err)
			}

			for _, dep := range updatedDeps {
				matches := re.FindStringSubmatch(dep.Name)
				if matches == nil {
					continue
				}

				logrus.Debugf("Matched dependency %s with %s", dep.Name, r.MatchDeps)

				var name string

				if len(matches) < 2 {
					name = path.Base(dep.Name)
				} else {
					name = matches[1]
				}

				if err = os.Chdir(gitRoot); err != nil {
					return fmt.Errorf("unable to chdir to temp clone directory: %w", err)
				}

				var cloned bool

				if _, err = os.Stat(name); err != nil && os.IsNotExist(err) {
					logrus.Debugf("git clone %s %s", dep.GitURL, name)

					if _, err = git("clone", dep.GitURL, name); err != nil {
						return fmt.Errorf("failed to clone: %w", err)
					}

					cloned = true
				} else if err != nil {
					return fmt.Errorf("unable to stat: %w", err)
				}

				if err = os.Chdir(name); err != nil {
					return fmt.Errorf("unable to chdir to cloned %s directory: %w", name, err)
				}

				if !cloned {
					if _, err = git("show", dep.Ref); err != nil {
						logrus.WithField("name", name).Debugf("git fetch origin")

						if _, err = git("fetch", "origin"); err != nil {
							return fmt.Errorf("failed to fetch: %w", err)
						}
					}
				}

				var changes []change

				changes, err = changelog(dep.Previous, dep.Ref)
				if err != nil {
					return fmt.Errorf("failed to get changelog for %s: %w", name, err)
				}

				if err = addContributors(dep.Previous, dep.Ref, contributors); err != nil {
					return fmt.Errorf("failed to get authors for %s: %w", name, err)
				}

				if linkify {
					if !strings.HasPrefix(dep.Name, "github.com/") {
						logrus.Debugf("linkify only supported for Github, skipping %s", dep.Name)
					} else {
						ghname := dep.Name[11:]

						if err = linkifyChanges(changes, githubCommitLink(ghname, gfm), githubPRLink(ghname), gfm); err != nil {
							return err
						}
					}
				}

				projectChanges = append(projectChanges, projectChange{
					Name:    name,
					Changes: changes,
				})
			}

			if err = os.Chdir(cwd); err != nil {
				return fmt.Errorf("unable to chdir to previous cwd: %w", err)
			}
		}

		// update the release fields with generated data
		r.Contributors = orderContributors(contributors)
		r.Dependencies = updatedDeps
		r.Changes = projectChanges
		r.Tag = tag
		r.Version = version

		if r.ReleaseDate == "" {
			r.ReleaseDate = time.Now().UTC().Format("2006-01-02")
		}

		// Remove trailing new lines
		r.Preface = strings.TrimRightFunc(r.Preface, unicode.IsSpace)

		tmpl, err := getTemplate(context)
		if err != nil {
			return err
		}

		if context.Bool("dry") {
			t, err := template.New("release-notes").Parse(tmpl)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 8, 8, 2, ' ', 0)
			if err := t.Execute(w, r); err != nil {
				return err
			}

			return w.Flush()
		}

		logrus.Info("release complete!")

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
