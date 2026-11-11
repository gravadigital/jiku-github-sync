package github

import (
	"context"
	"time"
)

// ProjectSchema is a Project v2 and its single-select fields, by name.
type ProjectSchema struct {
	ID     string
	Title  string
	Fields map[string]*SelectField
}

// SelectField is a single-select field with its options (name -> id).
type SelectField struct {
	ID      string
	Name    string
	Options map[string]string
	// Order keeps the options in the Project's order, for reports.
	Order []string
}

// Item is an issue's item in a Project, with its single-select values (field name -> option).
type Item struct {
	ID            string
	ContentNodeID string
	Values        map[string]FieldValue
}

// FieldValue is the value of a single-select field on an item.
type FieldValue struct {
	Option    string
	UpdatedAt time.Time
	// Creator is who set it last; "github-project-automation" for the built-in workflows.
	Creator string
}

// ProjectSchema reads a Project's single-select fields.
func (c *Client) ProjectSchema(ctx context.Context, projectID string) (*ProjectSchema, error) {
	var out struct {
		Node *struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Fields struct {
				Nodes []struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					Options []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"options"`
				} `json:"nodes"`
			} `json:"fields"`
		} `json:"node"`
	}
	q := `query($id:ID!){node(id:$id){... on ProjectV2{id title fields(first:100){nodes{... on ProjectV2SingleSelectField{id name options{id name}}}}}}}`
	if err := c.graphql(ctx, q, map[string]any{"id": projectID}, &out, false); err != nil {
		return nil, err
	}
	if out.Node == nil || out.Node.ID == "" {
		return nil, &APIError{Status: 404, Method: "POST", Path: "/graphql", Message: "project " + projectID}
	}
	s := &ProjectSchema{ID: out.Node.ID, Title: out.Node.Title, Fields: map[string]*SelectField{}}
	for _, f := range out.Node.Fields.Nodes {
		if f.ID == "" {
			continue // not a single-select field
		}
		sf := &SelectField{ID: f.ID, Name: f.Name, Options: map[string]string{}}
		for _, o := range f.Options {
			sf.Options[o.Name] = o.ID
			sf.Order = append(sf.Order, o.Name)
		}
		s.Fields[f.Name] = sf
	}
	return s, nil
}

const itemFields = `id content{... on Issue{id}} fieldValues(first:50){nodes{... on ProjectV2ItemFieldSingleSelectValue{name updatedAt creator{login} field{... on ProjectV2SingleSelectField{name}}}}}`

type gqlItem struct {
	ID      string `json:"id"`
	Project *struct {
		ID string `json:"id"`
	} `json:"project"`
	Content *struct {
		ID string `json:"id"`
	} `json:"content"`
	FieldValues struct {
		Nodes []struct {
			Name      string    `json:"name"`
			UpdatedAt time.Time `json:"updatedAt"`
			Creator   *struct {
				Login string `json:"login"`
			} `json:"creator"`
			Field *struct {
				Name string `json:"name"`
			} `json:"field"`
		} `json:"nodes"`
	} `json:"fieldValues"`
}

func (g *gqlItem) toItem() *Item {
	it := &Item{ID: g.ID, Values: map[string]FieldValue{}}
	if g.Content != nil {
		it.ContentNodeID = g.Content.ID
	}
	for _, v := range g.FieldValues.Nodes {
		if v.Field == nil || v.Field.Name == "" {
			continue
		}
		fv := FieldValue{Option: v.Name, UpdatedAt: v.UpdatedAt}
		if v.Creator != nil {
			fv.Creator = v.Creator.Login
		}
		it.Values[v.Field.Name] = fv
	}
	return it
}

// ListItems lists every issue item of a Project (draft issues and pull requests are left out).
func (c *Client) ListItems(ctx context.Context, projectID string) ([]*Item, error) {
	var all []*Item
	var cursor any
	for {
		var out struct {
			Node *struct {
				Items struct {
					Nodes    []gqlItem `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"items"`
			} `json:"node"`
		}
		q := `query($id:ID!,$after:String){node(id:$id){... on ProjectV2{items(first:100,after:$after){pageInfo{hasNextPage endCursor} nodes{` + itemFields + `}}}}}`
		if err := c.graphql(ctx, q, map[string]any{"id": projectID, "after": cursor}, &out, false); err != nil {
			return nil, err
		}
		if out.Node == nil {
			return nil, &APIError{Status: 404, Method: "POST", Path: "/graphql", Message: "project " + projectID}
		}
		for i := range out.Node.Items.Nodes {
			it := out.Node.Items.Nodes[i].toItem()
			if it.ContentNodeID != "" {
				all = append(all, it)
			}
		}
		if !out.Node.Items.PageInfo.HasNextPage {
			return all, nil
		}
		cursor = out.Node.Items.PageInfo.EndCursor
	}
}

// ItemForIssue returns the issue's item in the given Project, or nil when it is not there.
func (c *Client) ItemForIssue(ctx context.Context, issueNodeID, projectID string) (*Item, error) {
	var out struct {
		Node *struct {
			ProjectItems struct {
				Nodes []gqlItem `json:"nodes"`
			} `json:"projectItems"`
		} `json:"node"`
	}
	q := `query($id:ID!){node(id:$id){... on Issue{projectItems(first:50,includeArchived:true){nodes{project{id} ` + itemFields + `}}}}}`
	if err := c.graphql(ctx, q, map[string]any{"id": issueNodeID}, &out, false); err != nil {
		return nil, err
	}
	if out.Node == nil {
		return nil, nil
	}
	for i := range out.Node.ProjectItems.Nodes {
		g := &out.Node.ProjectItems.Nodes[i]
		if g.Project != nil && g.Project.ID == projectID {
			it := g.toItem()
			it.ContentNodeID = issueNodeID
			return it, nil
		}
	}
	return nil, nil
}

// AddItem adds an issue to a Project and returns the item id (the existing one if it was there).
func (c *Client) AddItem(ctx context.Context, projectID, contentNodeID string) (string, error) {
	var out struct {
		Add struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	q := `mutation($p:ID!,$c:ID!){addProjectV2ItemById(input:{projectId:$p,contentId:$c}){item{id}}}`
	if err := c.graphql(ctx, q, map[string]any{"p": projectID, "c": contentNodeID}, &out, true); err != nil {
		return "", err
	}
	return out.Add.Item.ID, nil
}

// SetSelect sets a single-select field of an item.
func (c *Client) SetSelect(ctx context.Context, projectID, itemID, fieldID, optionID string) error {
	q := `mutation($p:ID!,$i:ID!,$f:ID!,$o:String!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{singleSelectOptionId:$o}}){projectV2Item{id}}}`
	return c.graphql(ctx, q, map[string]any{"p": projectID, "i": itemID, "f": fieldID, "o": optionID}, nil, false)
}

// ClearField empties a field of an item.
func (c *Client) ClearField(ctx context.Context, projectID, itemID, fieldID string) error {
	q := `mutation($p:ID!,$i:ID!,$f:ID!){clearProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f}){projectV2Item{id}}}`
	return c.graphql(ctx, q, map[string]any{"p": projectID, "i": itemID, "f": fieldID}, nil, false)
}

// StatusChange is the last change of an issue's Status in a Project, from the issue's timeline.
// It is the only place GitHub says WHO moved a card: an item field value's `creator` is whoever
// set it first and does not change on later edits.
type StatusChange struct {
	Actor     string
	Status    string
	CreatedAt time.Time
}

// LastStatusChange returns the most recent Status change of the issue in projectID, or nil.
func (c *Client) LastStatusChange(ctx context.Context, issueNodeID, projectID string) (*StatusChange, error) {
	var out struct {
		Node *struct {
			TimelineItems struct {
				Nodes []struct {
					CreatedAt time.Time `json:"createdAt"`
					Status    string    `json:"status"`
					Actor     *struct {
						Login string `json:"login"`
					} `json:"actor"`
					Project *struct {
						ID string `json:"id"`
					} `json:"project"`
				} `json:"nodes"`
			} `json:"timelineItems"`
		} `json:"node"`
	}
	q := `query($id:ID!){node(id:$id){... on Issue{timelineItems(last:25,itemTypes:[PROJECT_V2_ITEM_STATUS_CHANGED_EVENT]){nodes{... on ProjectV2ItemStatusChangedEvent{createdAt status actor{login} project{id}}}}}}}`
	if err := c.graphql(ctx, q, map[string]any{"id": issueNodeID}, &out, false); err != nil {
		return nil, err
	}
	if out.Node == nil {
		return nil, nil
	}
	ns := out.Node.TimelineItems.Nodes
	for i := len(ns) - 1; i >= 0; i-- {
		n := ns[i]
		if n.Project == nil || n.Project.ID != projectID {
			continue
		}
		sc := &StatusChange{Status: n.Status, CreatedAt: n.CreatedAt}
		if n.Actor != nil {
			sc.Actor = n.Actor.Login
		}
		return sc, nil
	}
	return nil, nil
}
