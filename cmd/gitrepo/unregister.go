package gitrepo

import (
	"encoding/json"
	"fmt"
	"github.com/argoproj/argo-cd/v2/util/localconfig"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
)

func unregister() *cobra.Command {
	var (
		repoAlias string
	)
	command := &cobra.Command{
		Use:   "unregister",
		Args:  cobra.ExactArgs(1),
		Short: "unregister a previously registered configuration",
		Long:  "unregister a previously registered configuration",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			repoAlias = args[0]
			cfgDir, err := localconfig.DefaultConfigDir()
			if err != nil {
				err = fmt.Errorf("cannot open config file, error: %w", err)
				return
			}
			cfgFile := filepath.Join(cfgDir, repoCtxFile)
			file, err := os.OpenFile(cfgFile, os.O_RDWR|os.O_CREATE, 0666)
			if err != nil {
				err = fmt.Errorf("cannot open config file, error: %w", err)
				return
			}
			defer func(f *os.File) {
				err := f.Close()
				if err != nil {
					fmt.Printf("failed to close config file, error: %v\n", err)
				}
			}(file)
			content, err := io.ReadAll(file)
			if err != nil {
				err = fmt.Errorf("cannot read config file, error: %w", err)
				return
			}
			if len(content) == 0 {
				fmt.Println("no repositories registered")
				return
			}
			var repoCtxCfg RepoCtxCfg
			if err = json.Unmarshal(content, &repoCtxCfg); err != nil {
				err = fmt.Errorf("cannot open config file, error: %w", err)
				return
			}
			for i, repo := range repoCtxCfg.Repos {
				if repo.Alias != repoAlias {
					continue
				}
				if repo.Alias == repoDefaultCtx && repoCtxCfg.Current.Alias == repoDefaultCtx {
					repoCtxCfg.Current = RepoCtx{}
				}
				repoCtxCfg.Repos = append(repoCtxCfg.Repos[:i], repoCtxCfg.Repos[i+1:]...)
				repoData, err := json.MarshalIndent(repoCtxCfg, "", "\t")
				if err != nil {
					return fmt.Errorf("cannot serialize repo context, error: %w", err)
				}
				if err := file.Truncate(0); err != nil {
					return fmt.Errorf("cannot overwrite config file, error: %w", err)
				}
				if _, err := file.Seek(0, 0); err != nil {
					return fmt.Errorf("cannot overwrite config file, error: %w", err)
				}
				_, err = file.Write(repoData)
				if err != nil {
					return err
				}
				fmt.Printf("Repository %s deleted\n", repoAlias)
				return nil
			}
			return
		},
	}
	return command
}
