package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
)

const taskXMLNamespace = "http://schemas.microsoft.com/windows/2004/02/mit/task"

// These are schema defaults, not a second task specification. Removing an
// explicit default makes it comparable to an omitted element in a native
// export. Non-default and unknown fields remain in the comparison.
// Sources: https://learn.microsoft.com/en-us/windows/win32/taskschd/task-scheduler-schema
// (settingsType, idleSettingsType, triggerBaseType, logonTriggerType),
// https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tsch/2e1de7c6-804e-40db-8aeb-9e0df2b9bb02
// (Volatile), and https://learn.microsoft.com/en-us/windows/win32/taskschd/security-contexts-for-running-tasks
// (the default LeastPrivilege run level).
// DeleteExpiredTaskAfter is deliberately excluded: omission means no deletion,
// despite its XSD default of PT0S. See https://learn.microsoft.com/en-us/windows/win32/taskschd/taskschedulerschema-deleteexpiredtaskafter-settingstype-element.
// Duration/WaitTimeout are inactive for the current writer's RunOnlyIfIdle=false
// tasks. Their normalization must be revisited before supporting idle-only specs.
var taskXMLDefaults = map[string]string{
	"Task/Settings/AllowStartOnDemand":                 "true",
	"Task/Settings/MultipleInstancesPolicy":            "IgnoreNew",
	"Task/Settings/DisallowStartIfOnBatteries":         "true",
	"Task/Settings/StopIfGoingOnBatteries":             "true",
	"Task/Settings/AllowHardTerminate":                 "true",
	"Task/Settings/StartWhenAvailable":                 "false",
	"Task/Settings/RunOnlyIfNetworkAvailable":          "false",
	"Task/Settings/WakeToRun":                          "false",
	"Task/Settings/Enabled":                            "true",
	"Task/Settings/Hidden":                             "false",
	"Task/Settings/ExecutionTimeLimit":                 "PT72H",
	"Task/Settings/Priority":                           "7",
	"Task/Settings/RunOnlyIfIdle":                      "false",
	"Task/Settings/UseUnifiedSchedulingEngine":         "false",
	"Task/Settings/DisallowStartOnRemoteAppSession":    "false",
	"Task/Settings/Volatile":                           "false",
	"Task/Settings/IdleSettings/Duration":              "PT10M",
	"Task/Settings/IdleSettings/WaitTimeout":           "PT1H",
	"Task/Settings/IdleSettings/StopOnIdleEnd":         "true",
	"Task/Settings/IdleSettings/RestartOnIdle":         "false",
	"Task/Triggers/LogonTrigger/Enabled":               "true",
	"Task/Triggers/LogonTrigger/ExecutionTimeLimit":    "PT72H",
	"Task/Triggers/LogonTrigger/Delay":                 "PT0S",
	"Task/Triggers/CalendarTrigger/Enabled":            "true",
	"Task/Triggers/CalendarTrigger/ExecutionTimeLimit": "PT72H",
	"Task/Triggers/CalendarTrigger/RandomDelay":        "PT0S",
	"Task/Principals/Principal/RunLevel":               "LeastPrivilege",
}

type taskDefinitionXML struct {
	XMLName  xml.Name
	Attrs    []xml.Attr          `xml:",any,attr"`
	Text     string              `xml:",chardata"`
	Children []taskDefinitionXML `xml:",any"`
}

// taskDefinitionsEquivalent compares the complete definition produced by the
// existing task writer, allowing only known metadata and schema defaults to
// differ. It is a read-only comparison, never proof of registration ownership.
func taskDefinitionsEquivalent(observed, expected []byte) (bool, error) {
	actual, err := decodeTaskDefinitionXML(observed)
	if err != nil {
		return false, err
	}
	want, err := decodeTaskDefinitionXML(expected)
	if err != nil {
		return false, err
	}
	normalizeTaskDefinitionXML(&actual, "Task", false)
	normalizeTaskDefinitionXML(&want, "Task", false)
	return reflect.DeepEqual(actual, want), nil
}

func decodeTaskDefinitionXML(raw []byte) (taskDefinitionXML, error) {
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
	dec.CharsetReader = func(charset string, reader io.Reader) (io.Reader, error) {
		// COM returns UTF-8 bytes while retaining the UTF-16 declaration.
		if !strings.EqualFold(charset, "UTF-16") {
			return nil, ErrTaskCorrupt
		}
		return reader, nil
	}
	var task taskDefinitionXML
	if err := dec.Decode(&task); err != nil || task.XMLName != (xml.Name{Space: taskXMLNamespace, Local: "Task"}) {
		return taskDefinitionXML{}, fmt.Errorf("%w: invalid task definition XML", ErrTaskCorrupt)
	}
	for {
		token, err := dec.Token()
		if err == io.EOF {
			return task, nil
		}
		if err != nil {
			return taskDefinitionXML{}, fmt.Errorf("%w: invalid task definition XML", ErrTaskCorrupt)
		}
		switch value := token.(type) {
		case xml.Comment:
			continue
		case xml.CharData:
			if len(bytes.TrimSpace(value)) == 0 {
				continue
			}
		}
		return taskDefinitionXML{}, fmt.Errorf("%w: trailing task definition XML", ErrTaskCorrupt)
	}
}

func normalizeTaskDefinitionXML(node *taskDefinitionXML, path string, mayOmit bool) bool {
	attrs := node.Attrs[:0]
	for _, attr := range node.Attrs {
		if attr.Name.Space != "xmlns" && attr.Name != (xml.Name{Local: "xmlns"}) {
			attrs = append(attrs, attr)
		}
	}
	node.Attrs = attrs
	if len(node.Attrs) == 0 {
		node.Attrs = nil
	}
	slices.SortFunc(node.Attrs, func(a, b xml.Attr) int {
		if order := strings.Compare(a.Name.Space, b.Name.Space); order != 0 {
			return order
		}
		return strings.Compare(a.Name.Local, b.Name.Local)
	})

	counts := make(map[xml.Name]int, len(node.Children))
	for _, child := range node.Children {
		counts[child.XMLName]++
	}
	children := node.Children[:0]
	for _, child := range node.Children {
		// Duplicated defaults/metadata must not disappear during normalization.
		if normalizeTaskDefinitionXML(&child, path+"/"+child.XMLName.Local, counts[child.XMLName] == 1) {
			children = append(children, child)
		}
	}
	node.Children = children
	if len(node.Children) == 0 {
		node.Children = nil
	}
	// These schema containers have unordered fields. Action order is retained.
	switch path {
	case "Task", "Task/RegistrationInfo", "Task/Settings", "Task/Settings/IdleSettings",
		"Task/Principals/Principal", "Task/Actions/Exec", "Task/Settings/RestartOnFailure",
		"Task/Triggers/CalendarTrigger/ScheduleByWeek", "Task/Triggers/CalendarTrigger/ScheduleByWeek/DaysOfWeek":
		slices.SortStableFunc(node.Children, func(a, b taskDefinitionXML) int {
			if order := strings.Compare(a.XMLName.Space, b.XMLName.Space); order != 0 {
				return order
			}
			return strings.Compare(a.XMLName.Local, b.XMLName.Local)
		})
	}
	if path != "Task/Actions/Exec/Arguments" {
		node.Text = strings.TrimSpace(node.Text)
	}
	if node.XMLName.Space != taskXMLNamespace || len(node.Attrs) != 0 || len(node.Children) != 0 {
		return true
	}
	if defaultValue, known := taskXMLDefaults[path]; known {
		if defaultValue == "true" || defaultValue == "false" {
			switch node.Text {
			case "1":
				node.Text = "true"
			case "0":
				node.Text = "false"
			}
		}
		if defaultValue == "PT0S" && (node.Text == "PT0M" || node.Text == "PT0H" || node.Text == "P0D") {
			node.Text = "PT0S"
		}
		if mayOmit && node.Text == defaultValue {
			return false
		}
	}
	switch path {
	case "Task/Actions/Exec/Command", "Task/Actions/Exec/WorkingDirectory",
		"Task/Principals/Principal/UserId", "Task/Triggers/LogonTrigger/UserId":
		node.Text = strings.ToLower(node.Text)
	case "Task/RegistrationInfo/Date", "Task/RegistrationInfo/Author", "Task/RegistrationInfo/Description",
		"Task/RegistrationInfo/URI", "Task/RegistrationInfo/Source", "Task/RegistrationInfo/Version", "Task/RegistrationInfo/Documentation":
		return !mayOmit
	}
	if mayOmit && node.Text == "" {
		switch path {
		case "Task/RegistrationInfo", "Task/Settings/IdleSettings", "Task/Settings/NetworkSettings",
			"Task/Actions/Exec/Arguments", "Task/Actions/Exec/WorkingDirectory":
			return false
		}
	}
	return true
}
