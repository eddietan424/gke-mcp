// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/GoogleCloudPlatform/gke-mcp/pkg/config"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	_ "google.golang.org/genproto/googleapis/cloud/audit" // Import for AuditLog proto so we can convert to JSON.
	"google.golang.org/protobuf/encoding/protojson"
)

type LogQueryRequest struct {
	Query     string     `json:"query"`
	ProjectID string     `json:"project_id"`
	TimeRange *TimeRange `json:"time_range,omitempty"`
	Since     string     `json:"since,omitempty"`
	Limit     int        `json:"limit,omitempty"`
	Format    string     `json:"format,omitempty"`
}

type TimeRange struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
}

const (
	defaultLimit = 10
	maxLimit     = 100
)

func installQueryLogsTool(s *server.MCPServer, conf *config.Config) {
	queryLogsTool := mcp.NewTool("query_logs",
		mcp.WithDescription("Query Google Cloud Platform logs using Logging Query Language (LQL). Before using this tool, it's **strongly** recommended to call the 'get_log_schema' tool to get information about supported log types and their schemas. Logs are returned in ascending order, based on the timestamp (i.e. oldest first)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("project_id", mcp.Description("GCP project ID to query logs from. Required."), mcp.Required()),
		mcp.WithString("query", mcp.Description("LQL query string to filter and retrieve log entries. Don't specify time ranges in this filter. Use 'time_range' instead.")),
		mcp.WithObject("time_range", mcp.Description("Time range for log query. If empty, no restrictions are applied."),
			mcp.Properties(map[string]any{
				"start_time": map[string]any{
					"type":        "string",
					"description": "Start time for log query (RFC3339 format)",
				},
				"end_time": map[string]any{
					"type":        "string",
					"description": "End time for log query (RFC3339 format)",
				},
			}),
		),
		mcp.WithString("since", mcp.Description("Only return logs newer than a relative duration like 5s, 2m, or 3h. The only supported units are seconds ('s'), minutes ('m'), and hours ('h').")),
		mcp.WithNumber("limit", mcp.Description(fmt.Sprintf("Maximum number of log entries to return. Cannot be greater than %d. Consider multiple calls if needed. Defaults to %d.", maxLimit, defaultLimit))),
		mcp.WithString("format", mcp.Description("Go template string to format each log entry. If empty, the full JSON representation is returned. Note that empty fields are not included in the response. Example: '{{.timestamp}} [{{.severity}}] {{.textPayload}}'. It's strongly recommended to use a template to minimize the size of the response and only include the fields you need. Use the get_schema tool before this tool to get information about supported log types and their schemas.")),
	)

	t := newQueryLogsTool(conf)
	s.AddTool(queryLogsTool, mcp.NewTypedToolHandler(t.queryLogs))
}

type queryLogsTool struct {
	conf *config.Config
}

func newQueryLogsTool(conf *config.Config) *queryLogsTool {
	return &queryLogsTool{
		conf: conf,
	}
}

func (t *queryLogsTool) queryLogs(ctx context.Context, _ mcp.CallToolRequest, req LogQueryRequest) (*mcp.CallToolResult, error) {
	req.setDefaults()
	if errMsg := req.validate(); errMsg != "" {
		return mcp.NewToolResultError(errMsg), nil
	}
	result, err := t.queryGCPLogs(ctx, req)
	if err != nil {
		return mcp.NewToolResultErrorf("Query failed: %v", err), nil
	}

	return mcp.NewToolResultText(result), nil
}

func (r *LogQueryRequest) setDefaults() {
	if r.Limit == 0 {
		r.Limit = defaultLimit
	}
}

func (r *LogQueryRequest) validate() string {
	if r.ProjectID == "" {
		return "project_id parameter is required"
	}
	if r.Limit > maxLimit {
		return fmt.Sprintf("limit parameter cannot be greater than %d", maxLimit)
	}
	if _, err := time.ParseDuration(r.Since); err != nil {
		return fmt.Sprintf("invalid since parameter: %v", err)
	}
	if r.TimeRange != nil && r.Since != "" {
		return "since parameter cannot be used with time_range"
	}
	if r.Format != "" {
		var err error
		_, err = template.New("log").Parse(r.Format)
		if err != nil {
			return fmt.Sprintf("invalid format template: %v", err)
		}
	}
	return ""
}

func (t *queryLogsTool) queryGCPLogs(ctx context.Context, req LogQueryRequest) (string, error) {
	client, err := logging.NewClient(context.TODO(), option.WithUserAgent(t.conf.UserAgent()))
	if err != nil {
		return "", fmt.Errorf("failed to create logging client: %v", err)
	}
	defer client.Close()

	listLogsReq := buildListLogEntriesRequest(req)
	// Request one more than the limit to check for truncation.
	listLogsReq.PageSize = int32(req.Limit + 1)

	resp := client.ListLogEntries(ctx, listLogsReq)

	var entries []*loggingpb.LogEntry
	for {
		entry, err := resp.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to iterate log entries: %v", err)
		}
		entries = append(entries, entry)
		if len(entries) > req.Limit {
			break
		}
	}

	truncated := len(entries) > req.Limit
	if truncated {
		entries = entries[:req.Limit]
	}

	allLogLines := strings.Builder{}
	if len(entries) == 0 {
		allLogLines.WriteString("No log entries found.")
	} else {
		formatter, err := formatterForRequest(req)
		if err != nil {
			return "", fmt.Errorf("failed to create formatter: %w", err)
		}

		for i, entry := range entries {
			if i > 0 {
				allLogLines.WriteString("\n")
			}
			logLine, err := formatter.format(entry)
			if err != nil {
				return "", fmt.Errorf("failed to format log entry: %w", err)
			}
			allLogLines.WriteString(logLine)
		}
	}

	result := fmt.Sprintf("Project ID: %s\nLQL Query:\n```\n%s\n```\nResult:\n\n%s", req.ProjectID, listLogsReq.Filter, allLogLines.String())
	if truncated {
		result += fmt.Sprintf("\n\nWarning: Results truncated. The query returned more than the limit of %d log entries. You can use the `limit` parameter to request more entries (up to %d).", req.Limit, maxLimit)
	}

	return result, nil
}

func buildListLogEntriesRequest(req LogQueryRequest) *loggingpb.ListLogEntriesRequest {
	filter := req.Query

	if req.Since != "" {
		since, err := time.ParseDuration(req.Since)
		if err != nil {
			return nil
		}
		req.TimeRange = &TimeRange{
			StartTime: time.Now().Add(-since),
		}
	}
	if req.TimeRange != nil {
		var timeFilters []string
		if !req.TimeRange.StartTime.IsZero() {
			timeFilters = append(timeFilters, fmt.Sprintf(`timestamp >= "%s"`, req.TimeRange.StartTime.Format(time.RFC3339)))
		}
		if !req.TimeRange.EndTime.IsZero() {
			timeFilters = append(timeFilters, fmt.Sprintf(`timestamp <= "%s"`, req.TimeRange.EndTime.Format(time.RFC3339)))
		}
		if len(timeFilters) > 0 {
			if filter != "" {
				filter += " AND "
			}
			filter += strings.Join(timeFilters, " AND ")
		}
	}
	return &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", req.ProjectID)},
		Filter:        filter,
		PageSize:      int32(req.Limit),
		OrderBy:       "timestamp asc",
	}
}

func formatterForRequest(req LogQueryRequest) (formatter, error) {
	if req.Format == "" {
		return &jsonFormatter{}, nil
	}

	tmpl, err := template.New("log").Parse(req.Format)
	if err != nil {
		return nil, fmt.Errorf("failed to parse format template: %w", err)
	}
	return &goTemplateFormatter{tmpl: tmpl}, nil
}

type formatter interface {
	format(entry *loggingpb.LogEntry) (string, error)
}

type jsonFormatter struct{}

func (f *jsonFormatter) format(entry *loggingpb.LogEntry) (string, error) {
	m := protojson.MarshalOptions{
		Multiline:       true,
		Indent:          "  ",
		EmitUnpopulated: false,
	}
	logLine, err := m.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("could not marshal log entry to JSON: %w", err)
	}
	return string(logLine), nil
}

type goTemplateFormatter struct {
	tmpl *template.Template
}

func (f *goTemplateFormatter) format(entry *loggingpb.LogEntry) (string, error) {
	b, err := protojson.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("could not marshal log entry to JSON for template: %w", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(b, &data); err != nil {
		return "", fmt.Errorf("could not unmarshal log entry to map for template: %w", err)
	}
	var logLine strings.Builder
	if err := f.tmpl.Execute(&logLine, data); err != nil {
		return "", err
	}
	return logLine.String(), nil
}
