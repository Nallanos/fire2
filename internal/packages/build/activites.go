package build

import (
	"context"
	"os"

	"github.com/go-git/go-git/v6"
)

type Activities struct {
	repo Repository
}

func NewActivities(repo Repository) *Activities {
	return &Activities{repo: repo}
}

func (a *Activities) CreateBuild(ctx context.Context, b Build) (Build, error) {
	return a.repo.Create(ctx, b)
}

func (a *Activities) CloneRepository(ctx context.Context, opts *git.CloneOptions) (*git.Repository, error) {
	file, _ := os.MkdirTemp("", "repo-*")

	return git.PlainCloneContext(ctx, file, opts)
}
