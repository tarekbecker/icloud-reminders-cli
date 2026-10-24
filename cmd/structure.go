package cmd

import (
	"encoding/json"
	"time"

	"github.com/spf13/cobra"
	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/reminders"
)

func structureService() *reminders.Service { return reminders.New(cache.ConfigDir, 3*time.Minute) }
func printStructureResult(command *cobra.Command, result any, err error) error {
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func init() {
	var sectionList, sectionTitle string
	sections := &cobra.Command{Use: "sections", Short: "List native section headings (exact list ID required)", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		r, e := structureService().Sections(c.Context(), reminders.SectionsInput{ListID: sectionList})
		return printStructureResult(c, r, e)
	}}
	sections.PersistentFlags().StringVar(&sectionList, "list", "", "Exact list ID")
	_ = sections.MarkPersistentFlagRequired("list")
	addSection := &cobra.Command{Use: "add", Short: "Create a native section heading", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		r, e := structureService().CreateSection(c.Context(), reminders.CreateSectionInput{ListID: sectionList, Title: sectionTitle})
		return printStructureResult(c, r, e)
	}}
	addSection.Flags().StringVar(&sectionTitle, "title", "", "Section heading")
	_ = addSection.MarkFlagRequired("title")
	sections.AddCommand(addSection)
	var moveInput reminders.MoveInput
	move := &cobra.Command{Use: "move <reminder-id>", Short: "Change parent, native section or manual position in the same list", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		moveInput.ID = args[0]
		r, e := structureService().Move(c.Context(), moveInput)
		return printStructureResult(c, r, e)
	}}
	move.Flags().StringVar(&moveInput.ParentID, "parent", "", "Exact parent ID (indent)")
	move.Flags().BoolVar(&moveInput.ClearParent, "clear-parent", false, "Make top-level")
	move.Flags().StringVar(&moveInput.SectionID, "section", "", "Exact section ID")
	move.Flags().BoolVar(&moveInput.ClearSection, "clear-section", false, "Move out of a section")
	move.Flags().StringVar(&moveInput.BeforeID, "before", "", "Place before this exact sibling ID")
	move.Flags().StringVar(&moveInput.AfterID, "after", "", "Place after this exact sibling ID")
	move.MarkFlagsMutuallyExclusive("parent", "clear-parent")
	move.MarkFlagsMutuallyExclusive("section", "clear-section")
	move.MarkFlagsMutuallyExclusive("before", "after")
	var reorderInput reminders.ReorderInput
	reorder := &cobra.Command{Use: "reorder <sibling-id>...", Short: "Set manual order; include every sibling exactly once, including completed reminders", Args: cobra.MinimumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		reorderInput.ReminderIDs = args
		r, e := structureService().Reorder(c.Context(), reorderInput)
		return printStructureResult(c, r, e)
	}}
	reorder.Flags().StringVar(&reorderInput.ListID, "list", "", "Exact list ID")
	reorder.Flags().StringVar(&reorderInput.ParentID, "parent", "", "Exact parent ID; omit for top-level")
	reorder.Flags().StringVar(&reorderInput.SectionID, "section", "", "Exact section ID; omit for unsectioned top-level")
	_ = reorder.MarkFlagRequired("list")
	RootCmd.AddCommand(sections, move, reorder)
}
