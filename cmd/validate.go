// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ory/x/logrusx"

	"github.com/ory/oathkeeper/driver"
	"github.com/ory/oathkeeper/rule"
	"github.com/ory/oathkeeper/x"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the configuration and access rules",
	Long: `Validates the Oathkeeper configuration file and all configured access rules
without starting the server. Exits with code 0 if the configuration is valid,
or with code 1 if any errors are found.

Use this command as a pre-flight check before deploying or in CI/CD pipelines
to catch configuration errors early.`,
	Run: func(cmd *cobra.Command, args []string) {
		logger := logrusx.New("ORY Oathkeeper", x.Version)
		fmt.Println("Validating Oathkeeper configuration...")

		// Load configuration using the same code path as serve
		d := driver.NewDefaultDriver(logger, x.Version, "", "", cmd.Root().PersistentFlags())

		// List the configured repositories for visibility
		repos := d.Configuration().AccessRuleRepositories()
		fmt.Printf("\nConfigured repositories (%d):\n", len(repos))
		for i, repo := range repos {
			fmt.Printf("  [%d] %s\n", i, repo.String())
		}

		// Validate configuration and rules
		if err := d.Registry().ValidateAndInit(); err != nil {
			fmt.Fprintf(os.Stderr, "\n❌ Configuration validation failed:\n   %v\n", err)
			fmt.Fprintf(os.Stderr, "\n🔍 What to check:\n")
			fmt.Fprintf(os.Stderr, "   • Does the configuration file exist and is it valid YAML/JSON?\n")
			fmt.Fprintf(os.Stderr, "   • Do all access_rules.repositories paths/URLs exist and are accessible?\n")
			fmt.Fprintf(os.Stderr, "   • Is the file readable by the oathkeeper process?\n")
			os.Exit(1)
		}

		// Check rule counts
		count, err := d.Registry().RuleRepository().Count(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n❌ Failed to count loaded rules:\n   %v\n", err)
			os.Exit(1)
		}

		if len(repos) > 0 && count == 0 {
			fmt.Fprintf(os.Stderr, "\n❌ No valid rules loaded from %d configured repositories.\n", len(repos))
			fmt.Fprintf(os.Stderr, "\n🔍 What to check:\n")
			fmt.Fprintf(os.Stderr, "   • Are the rule files valid YAML/JSON arrays of rule objects?\n")
			fmt.Fprintf(os.Stderr, "   • Does each rule have: id, match.url, authenticators, authorizer, mutators?\n")
			fmt.Fprintf(os.Stderr, "   • Are all referenced handlers (authenticators, authorizers, mutators, errors)\n")
			fmt.Fprintf(os.Stderr, "     enabled in the main configuration file?\n")
			os.Exit(1)
		}

		// Check for invalid rules and enumerate them
		if repo, ok := d.Registry().RuleRepository().(*rule.RepositoryMemory); ok {
			invalidRules := repo.InvalidRules()
			if len(invalidRules) > 0 {
				fmt.Fprintf(os.Stderr, "\n❌ %d of %d rule(s) failed validation:\n", len(invalidRules), count+len(invalidRules))
				for _, r := range invalidRules {
					matchURL := "<no match configured>"
					if r.Match != nil {
						matchURL = r.Match.GetURL()
					}
					fmt.Fprintf(os.Stderr, "\n   Rule ID:    %s\n", r.ID)
					fmt.Fprintf(os.Stderr, "   Match URL:  %s\n", matchURL)
					// Re-validate to get the specific error message for this rule
					if verr := d.Registry().RuleValidator().Validate(&r); verr != nil {
						fmt.Fprintf(os.Stderr, "   Error:      %v\n", verr)
					}
				}
				fmt.Fprintf(os.Stderr, "\n🔍 Common causes of rule validation failures:\n")
				fmt.Fprintf(os.Stderr, "   • Handler not enabled: ensure the handler is set to enabled: true in config\n")
				fmt.Fprintf(os.Stderr, "   • Handler not found: check for typos in handler names\n")
				fmt.Fprintf(os.Stderr, "   • Missing required fields: each rule needs match, authenticators, authorizer, mutators\n")
				fmt.Fprintf(os.Stderr, "   • Invalid upstream URL: must be a valid URL (e.g., http://backend:8080)\n")
				os.Exit(1)
			}
		}

		fmt.Printf("\n✅ Configuration is valid. %d rule(s) loaded successfully from %d repository/repositories.\n", count, len(repos))
	},
}

func init() {
	RootCmd.AddCommand(validateCmd)
}
