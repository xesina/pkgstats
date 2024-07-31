package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"github.com/google/go-github/v63/github"
	"github.com/samber/lo"
	"golang.org/x/mod/modfile"
	"golang.org/x/oauth2"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := run(ctx); err != nil {
			log.Fatalf("error: %v", err)
		}
	}()

	// Wait for the application to finish
	<-ctx.Done()

	// Signal received, perform cleanup
	fmt.Println("Received shutdown signal, stopping search...")

	// Wait for the goroutine to finish
	wg.Wait()

	// Perform any final cleanup or resource release here
	fmt.Println("Graceful shutdown complete.")
}

func run(ctx context.Context) error {
	var (
		packageName string
		githubToken string
	)

	// get package name as flag
	flag.StringVar(&packageName, "pkg", "", "package name to search for")
	flag.StringVar(&githubToken, "token", "", "GitHub access token for authentication")

	flag.Parse()

	if packageName == "" || githubToken == "" {
		return fmt.Errorf("missing package name or GitHub access token")
	}

	// create a cache directory if it doesn't exist
	_, err := os.Stat("cache")
	if os.IsNotExist(err) {
		err := os.Mkdir("cache", 0755)
		if err != nil {
			return fmt.Errorf("error creating cache directory: %v", err)
		}
	}

	filename := strings.ReplaceAll(packageName, "/", "-")
	fileName := fmt.Sprintf("cache/%s.csv", filename)

	file, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()

	// read csv file to check if the package has already been searched for
	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("error reading file: %v", err)
	}

	// create a map to store the cache
	results := make(map[string]repoResult)
	for _, record := range records {
		stars, err := strconv.Atoi(record[2])
		if err != nil {
			return fmt.Errorf("invalid value for star count: %v", stars)
		}
		results[record[0]] = repoResult{
			name:  record[0],
			used:  record[1] == "true",
			stars: stars,
		}
	}

	// Set up GitHub client with authentication
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: githubToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// For debugging
	//tc := &oauth2.Transport{Source: ts, Base: dbg.New()}
	//client := github.NewClient(&http.Client{Transport: tc})

	// Create a search result object
	s := newSearchResult(packageName, client, results)
	newResults, err := s.Search(
		ctx,
		"language:go stars:>1000",
		&github.SearchOptions{
			Sort:  "stars",
			Order: "desc",
			ListOptions: github.ListOptions{
				PerPage: 50,
			},
		},
	)
	if err != nil {
		return fmt.Errorf("error searching: %v", err)
	}

	// merge the results
	for repo, repoResult := range newResults {
		if _, ok := results[repo]; !ok {
			results[repo] = repoResult
		}
	}

	// turn map into slice and sort it by star counts descending order
	sortedResults := lo.MapToSlice(results, func(k string, v repoResult) repoResult {
		return v
	})

	// Sort the slice by the Value field
	sort.Slice(sortedResults, func(i, j int) bool {
		return sortedResults[i].stars > sortedResults[j].stars
	})

	// replace the file with the new cache
	err = file.Truncate(0)
	if err != nil {
		return fmt.Errorf("error truncating file: %v", err)
	}
	fmt.Printf("truncated the file: %s\n", fileName)

	_, err = file.Seek(0, 0)
	if err != nil {
		return fmt.Errorf("error seeking file: %v", err)
	}
	fmt.Printf("seeked to the beginning of the file: %s\n", fileName)

	writer := csv.NewWriter(file)

	for _, repoResult := range sortedResults {
		foundStr := "false"
		if repoResult.used {
			foundStr = "true"
		}
		err := writer.Write([]string{repoResult.name, foundStr, strconv.Itoa(repoResult.stars)})
		if err != nil {
			return fmt.Errorf("error writing to file: %v", err)
		}
	}
	fmt.Printf("wrote to the file: %s\n", fileName)
	writer.Flush()
	fmt.Printf("flushed the writer\n")
	return nil
}

type repoResult struct {
	name  string
	used  bool
	stars int
}

type searchResult struct {
	client          *github.Client
	cache           map[string]repoResult
	packageName     string
	paginationDelay time.Duration
	searchDelay     time.Duration
}

func newSearchResult(packageName string, client *github.Client, results map[string]repoResult) *searchResult {
	const (
		defaultPaginationDelay = 7 * time.Second
		defaultSearchDelay     = 7 * time.Second
	)

	return &searchResult{
		cache:           results,
		client:          client,
		packageName:     packageName,
		paginationDelay: defaultPaginationDelay,
		searchDelay:     defaultSearchDelay,
	}
}

func (s *searchResult) Search(ctx context.Context, query string, opts *github.SearchOptions) (map[string]repoResult, error) {
	results := make(map[string]repoResult)

	for {
		select {
		case <-ctx.Done():
			// Stop the search if the context is canceled
			if errors.Is(ctx.Err(), context.Canceled) {
				fmt.Println("context canceled, stopping Search...")
				return results, nil
			}
			return results, ctx.Err()

		default:
			// Find matching repositories
			repos, resp, err := s.client.Search.Repositories(ctx, query, opts)
			if err != nil {
				return results, fmt.Errorf("error searching repositories: %v", err)
			}

			// Search in the repositories for the package usage
			repoSearchResults, err := s.searchInRepositories(ctx, repos)
			if err != nil {
				fmt.Printf("error searching the repositories: %v\n", err)
				continue
			}

			// update results
			for repo, found := range repoSearchResults {
				results[repo] = found
			}

			if resp.NextPage == 0 {
				break
			}

			fmt.Printf("Sleeping for %d seconds in Search\n", int(s.paginationDelay.Seconds()))
			if err := sleepWithContext(ctx, s.paginationDelay); err != nil {
				fmt.Printf("Sleep was interrupted: %v\n", err)
			}

			opts.Page = resp.NextPage
			fmt.Println("Searching next page: ", opts.Page)
		}
	}
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	select {
	case <-time.After(duration):
		// Sleep completed
		return nil
	case <-ctx.Done():
		// Context was canceled
		return ctx.Err()
	}
}

func (s *searchResult) searchInRepositories(ctx context.Context, repos *github.RepositoriesSearchResult) (map[string]repoResult, error) {
	results := make(map[string]repoResult)

	for _, repo := range repos.Repositories {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				fmt.Println("context canceled, stopping Search...")
				return results, nil
			}
			return results, ctx.Err()

		default:
			if repo.GetArchived() || repo.GetDisabled() || repo.GetFork() {
				fmt.Printf("Skipping arhived, disabled, forked repository: %s\n", repo.GetFullName())
				continue
			}

			if repoResult, ok := s.cache[repo.GetFullName()]; ok {
				previousStateStr := "not found"
				if repoResult.used {
					previousStateStr = "found"
				}
				fmt.Printf("Skipping repository: %s previously %s\n", repo.GetFullName(), previousStateStr)
				continue
			}

			fmt.Printf("Checking repository: %s\n", repo.GetFullName())

			// perform another search to find the package in the repository
			files, resp, err := s.client.Search.Code(
				ctx,
				fmt.Sprintf("%s repo:%s filename:go.mod", s.packageName, repo.GetFullName()),
				&github.SearchOptions{
					TextMatch: true,
				},
			)
			if err != nil {
				fmt.Printf("error searching repository: %s, error: %v\n", repo.GetFullName(), err)
				continue
			}

			fmt.Printf("searched repository: %s\n", repo.GetFullName())
			fmt.Printf("HTTP status code: %d, total files: %d\n", resp.StatusCode, files.GetTotal())

			repoSearchResult := repoResult{
				name:  repo.GetFullName(),
				stars: repo.GetStargazersCount(),
				used:  false,
			}

			for _, file := range files.CodeResults {
				// download the go.mod file
				reader, _, err := s.client.Repositories.DownloadContents(ctx, repo.GetOwner().GetLogin(), repo.GetName(), file.GetPath(), nil)
				if err != nil {
					fmt.Printf("error downloading go.mod file: %v\n", err)
					continue
				}

				// read from reader
				bb, err := io.ReadAll(reader)
				if err != nil {
					fmt.Printf("error reading go.mod file: %v\n", err)
					continue
				}

				if err := reader.Close(); err != nil {
					fmt.Printf("error closing reader: %v\n", err)
					continue
				}

				// parse the go.mod file
				f, err := modfile.Parse("go.mod", bb, nil)
				if err != nil {
					fmt.Printf("error parsing go.mod file: %v\n", err)
					continue
				}
				fmt.Printf("parsed go.mod file: %s\n", file.GetHTMLURL())

				// check if the package is in require section
				for _, require := range f.Require {
					// check if the package is in require section and not an indirect dependency
					if require.Mod.Path == s.packageName && !require.Indirect {
						fmt.Printf("Found package %s@%s in repository %s\n", s.packageName, require.Mod.Version, repo.GetFullName())
						repoSearchResult.used = true
						break
					}
				}

			}

			if !repoSearchResult.used {
				fmt.Printf("Package %s not found in repository %s\n", s.packageName, repo.GetFullName())
			}

			results[repo.GetFullName()] = repoSearchResult

			fmt.Printf("Sleeping for %d seconds in searchInRepositories\n", int(s.searchDelay.Seconds()))
			if err := sleepWithContext(ctx, s.searchDelay); err != nil {
				fmt.Printf("Sleep was interrupted: %v\n", err)
			}
		}
	}

	return results, nil
}
