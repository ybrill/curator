package repobuilder

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/tychoish/grip"
)

// TODO: in the future we may want to add an entry point or method for
// regenerating these pages throughout the tree, as the current
// integration for this function only regenerates pages on a very
// narrow swath (i.e. only the changed repos.)

func (c *RepositoryConfig) BuildIndexPageForDirectory(path, repoName string) error {
	tmpl, err := template.New("index").Parse(c.Templates.Index)
	if err != nil {
		return err
	}

	catcher := grip.NewCatcher()
	err = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// we want to write once index.html per directory. If
		// we don't have directory, we can't do anything here.
		if info.IsDir() {
			index, err := os.Create(filepath.Join(p, "index.html"))
			defer index.Close()
			catcher.Add(err)
			if err != nil {
				return nil
			}

			var contents []string
			numDirs := getNumDirs(p)

			err = filepath.Walk(p, func(contentPath string, info os.FileInfo, err error) error {
				// for each directory we walk its contents and add things to the listing for
				// that directory. This is not an optimal algorithm.

				if err != nil {
					return err
				}

				// skip listing "self"
				if contentPath == p {
					return nil
				}

				// don't list index.html files
				if strings.HasSuffix(contentPath, "index.html") {
					return nil
				}

				// we want to avoid list things recursively on each page. instead we only things if
				// it has one more element (i.e. a file name or sub directory) than the enclosing directory.
				if getNumDirs(contentPath)-1 == numDirs {
					contents = append(contents, filepath.Base(contentPath))
				}

				return nil
			})
			catcher.Add(err)

			err = tmpl.Execute(index, struct {
				Title    string
				RepoName string
				Files    []string
			}{
				Title:    fmt.Sprintf("Index of %s", filepath.Base(p)),
				RepoName: repoName,
				Files:    contents,
			})
			catcher.Add(err)

			grip.Noticeln("writing file at:", filepath.Join(p, "index.html"))
			return nil
		}
		return nil
	})
	catcher.Add(err)

	return catcher.Resolve()
}

func getNumDirs(path string) int {
	return len(strings.Split(path, string(os.PathSeparator)))
}
