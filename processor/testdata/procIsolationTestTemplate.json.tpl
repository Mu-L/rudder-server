{
{{- range $index, $workspace := .workspaces}}
    {{if $index }},{{ end }}
    "{{$workspace}}" : {
        "enableMetrics": false,
        "workspaceId": "{{$workspace}}",
        "sources": [
            {
                "config": {},
                "id": "{{$workspace}}",
                "name": "Dev Integration Test 1",
                "writeKey": "{{$workspace}}",
                "enabled": true,
                "sourceDefinitionId": "xxxyyyzzpWDzNxgGUYzq9sZdZZB",
                "createdBy": "xxxyyyzzueyoBz4jb7bRdOzDxai",
                "workspaceId": "{{$workspace}}",
                "deleted": false,
                "createdAt": "2021-08-27T06:33:00.305Z",
                "updatedAt": "2021-08-27T06:33:00.305Z",
                "destinations": [
                    {
                        "config": {
                            "webhookUrl": "{{$.webhookUrl}}",
                            "webhookMethod": "POST"
                        },
                        "secretConfig": {},
                        "id": "{{$.destinationId}}-{{$index}}",
                        "name": "Des WebHook Integration Test 1",
                        "enabled": true,
                        "workspaceId": "{{$workspace}}",
                        "deleted": false,
                        "createdAt": "2021-08-27T06:49:38.546Z",
                        "updatedAt": "2021-08-27T06:49:38.546Z",
                        "transformations": [
                            {
                                "versionId": "23hZ5VLGt0Yl7Hxk9ysBGi5baud",
                                "config": {},
                                "id": "23VJjP3bbHMzUhWjI7bvNL5S9xx"
                            }
                        ],
                        "destinationDefinition": {
                            "config": {
                                "destConfig": {
                                    "defaultConfig": [
                                        "webhookUrl",
                                        "webhookMethod",
                                        "headers"
                                    ]
                                },
                                "secretKeys": [
                                    "headers.to"
                                ],
                                "excludeKeys": [],
                                "includeKeys": [],
                                "transformAt": "processor",
                                "transformAtV1": "processor",
                                "supportedSourceTypes": [
                                    "android",
                                    "ios",
                                    "web",
                                    "unity",
                                    "amp",
                                    "cloud",
                                    "warehouse",
                                    "reactnative",
                                    "flutter"
                                ],
                                "supportedMessageTypes": [
                                    "alias",
                                    "group",
                                    "identify",
                                    "page",
                                    "screen",
                                    "track"
                                ],
                                "saveDestinationResponse": false
                            },
                            "configSchema": null,
                            "responseRules": null,
                            "id": "xxxyyyzzSOU9pLRavMf0GuVnWV3",
                            "name": "WEBHOOK",
                            "displayName": "Webhook",
                            "category": null,
                            "createdAt": "2020-03-16T19:25:28.141Z",
                            "updatedAt": "2021-08-26T07:06:01.445Z"
                        },
                        "isConnectionEnabled": true,
                        "isProcessorEnabled": true
                    }
                ],
                "sourceDefinition": {
                    "id": "xxxyyyzzpWDzNxgGUYzq9sZdZZB",
                    "name": "HTTP",
                    "options": null,
                    "displayName": "HTTP",
                    "category": "",
                    "createdAt": "2020-06-12T06:35:35.962Z",
                    "updatedAt": "2020-06-12T06:35:35.962Z"
                },
                "dgSourceTrackingPlanConfig": null
            }
        ],
        "libraries": []
    }
{{- end }}
}