// SPDX-License-Identifier: Apache-2.0

// Package all registers every supported platform; adding a provider means adding one import here.
package all

import (
	_ "github.com/basmulder03/repo-keeper/internal/provider/azuredevops" // azure devops services, server
	_ "github.com/basmulder03/repo-keeper/internal/provider/bitbucket"   // bitbucket cloud
	_ "github.com/basmulder03/repo-keeper/internal/provider/gitea"       // gitea, forgejo, codeberg
	_ "github.com/basmulder03/repo-keeper/internal/provider/github"      // github, GitHub Enterprise Server
	_ "github.com/basmulder03/repo-keeper/internal/provider/gitlab"      // gitlab.com, self-managed
)
