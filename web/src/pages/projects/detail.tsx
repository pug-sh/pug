import {
  type Project,
  UpdateDisplayNameRequestSchema,
  UpdateFCMServiceJSONRequestSchema
} from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { useEffect, useState } from 'react'
import { useParams } from 'wouter'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'
import { Spinner } from '@/components/ui/spinner'
import { projectsService } from '@/lib/rpc'

function ProjectDetail() {
  const { id } = useParams()
  const [project, setProject] = useState<Project | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [editingName, setEditingName] = useState(false)
  const [nameValue, setNameValue] = useState('')
  const [fcmJson, setFcmJson] = useState('')
  const [updating, setUpdating] = useState(false)

  useEffect(() => {
    const fetchProject = async () => {
      if (!id) return
      
      try {
        setLoading(true)
        const response = await projectsService.get({ id })
        if (response.project) {
          setProject(response.project)
          setNameValue(response.project.displayName)
          setFcmJson(response.project.fcmServiceJson || '')
        } else {
          setError('Project not found')
        }
      } catch (err) {
        if (err instanceof ConnectError) {
          setError(err.rawMessage)
        } else {
          setError(err instanceof Error ? err.message : 'An error occurred while fetching project')
        }
      } finally {
        setLoading(false)
      }
    }

    fetchProject()
  }, [id])

  const handleNameChange = async () => {
    if (!project || nameValue === project.displayName) {
      setEditingName(false)
      return
    }

    try {
      setUpdating(true)
      const request = create(UpdateDisplayNameRequestSchema, {
        displayName: nameValue,
        id: project.id
      })

      const response = await projectsService.updateDisplayName(request)
      if (response.project) {
        setProject(response.project)
      }
      setEditingName(false)
    } catch (err) {
      if (err instanceof ConnectError) {
        setError(err.rawMessage)
      } else {
        setError(err instanceof Error ? err.message : 'An error occurred while updating project name')
      }
    } finally {
      setUpdating(false)
    }
  }

  const handleCopyApiKey = () => {
    if (project) {
      navigator.clipboard.writeText(project.apiKey)
      alert('API Key copied to clipboard!')
    }
  }

  const handleUpdateFCMJson = async () => {
    if (!project) return

    try {
      setUpdating(true)
      const request = create(UpdateFCMServiceJSONRequestSchema, {
        fcmServiceJson: fcmJson,
        id: project.id
      })

      await projectsService.updateFCMServiceJSON(request)
      alert('FCM service JSON updated successfully!')
    } catch (err) {
      if (err instanceof ConnectError) {
        setError(err.rawMessage)
      } else {
        setError(err instanceof Error ? err.message : 'An error occurred while updating FCM service JSON')
      }
    } finally {
      setUpdating(false)
    }
  }

  if (loading) {
    return (
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
            <SidebarTrigger />
            <div className="flex-1">
              <h1 className="text-xl font-semibold">Project Details</h1>
            </div>
          </header>
          <main className="flex-1 p-4 sm:p-6 lg:p-8">
            <div className="max-w-4xl mx-auto flex justify-center items-center h-64">
              <Spinner className="h-8 w-8" />
            </div>
          </main>
        </SidebarInset>
      </SidebarProvider>
    )
  }

  if (error) {
    return (
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
            <SidebarTrigger />
            <div className="flex-1">
              <h1 className="text-xl font-semibold">Project Details</h1>
            </div>
          </header>
          <main className="flex-1 p-4 sm:p-6 lg:p-8">
            <div className="max-w-4xl mx-auto">
              <div className="p-4 text-destructive">
                <p>Error: {error}</p>
                <Button variant="outline" onClick={() => window.history.back()}>
                  Go Back
                </Button>
              </div>
            </div>
          </main>
        </SidebarInset>
      </SidebarProvider>
    )
  }

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Project Details</h1>
          </div>
        </header>
        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-4xl mx-auto space-y-6">
            {/* Project Info Card */}
            <Card>
              <CardHeader>
                <CardTitle>Project Information</CardTitle>
                <CardDescription>Basic information about your project</CardDescription>
              </CardHeader>
              <CardContent className="space-y-4">
                {editingName ? (
                  <div className="flex flex-col sm:flex-row gap-2 items-start sm:items-center">
                    <Input
                      value={nameValue}
                      onChange={(e) => setNameValue(e.target.value)}
                      className="flex-1"
                      placeholder="Project name"
                    />
                    <div className="flex gap-2">
                      <Button onClick={handleNameChange} disabled={updating}>
                        {updating ? 'Saving...' : 'Save'}
                      </Button>
                      <Button
                        variant="outline"
                        onClick={() => {
                          setEditingName(false)
                          setNameValue(project?.displayName || '')
                        }}
                        disabled={updating}
                      >
                        Cancel
                      </Button>
                    </div>
                  </div>
                ) : (
                  <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4">
                    <h2 className="text-xl font-bold">{project?.displayName}</h2>
                    <Button onClick={() => setEditingName(true)}>Edit Name</Button>
                  </div>
                )}
                
                <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                  <div>
                    <Label>Project ID</Label>
                    <div className="text-sm p-2 bg-muted rounded mt-1 break-all">
                      {project?.id}
                    </div>
                  </div>
                  
                  <div>
                    <Label>API Key</Label>
                    <div className="flex gap-2">
                      <Input
                        value={project?.apiKey || ''}
                        readOnly
                        className="font-mono text-xs"
                      />
                      <Button variant="outline" onClick={handleCopyApiKey}>
                        Copy
                      </Button>
                    </div>
                  </div>
                </div>
              </CardContent>
            </Card>

            {/* Firebase Integration Card */}
            <Card>
              <CardHeader>
                <CardTitle>Firebase Integration</CardTitle>
                <CardDescription>
                  Configure your Firebase service account JSON to enable push notifications
                </CardDescription>
              </CardHeader>
              <CardContent>
                <div className="space-y-4">
                  <div>
                    <Label htmlFor="fcmJson">Firebase Service Account JSON</Label>
                    <textarea
                      id="fcmJson"
                      value={fcmJson}
                      onChange={(e) => setFcmJson(e.target.value)}
                      rows={8}
                      className="w-full p-3 border rounded-md font-mono text-sm"
                      placeholder="Paste your Firebase service account JSON here..."
                    />
                  </div>
                  <div className="flex justify-end">
                    <Button onClick={handleUpdateFCMJson} disabled={updating}>
                      {updating ? 'Saving...' : 'Update Firebase Config'}
                    </Button>
                  </div>
                </div>
              </CardContent>
            </Card>

            {/* Future Integrations - Apple Push Notifications, VAPID, etc. */}
            <Card>
              <CardHeader>
                <CardTitle>Additional Integrations</CardTitle>
                <CardDescription>
                  Configure other services for your project
                </CardDescription>
              </CardHeader>
              <CardContent>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                  <Card className="p-4">
                    <CardTitle className="text-lg">Apple Push Notifications</CardTitle>
                    <p className="text-sm text-muted-foreground mt-2">
                      Configure your Apple Push Notification service credentials
                    </p>
                    <Button variant="outline" className="mt-4" disabled>
                      Configure
                    </Button>
                  </Card>
                  
                  <Card className="p-4">
                    <CardTitle className="text-lg">Web Push (VAPID)</CardTitle>
                    <p className="text-sm text-muted-foreground mt-2">
                      Set up VAPID keys for web push notifications
                    </p>
                    <Button variant="outline" className="mt-4" disabled>
                      Configure
                    </Button>
                  </Card>
                  
                  <Card className="p-4">
                    <CardTitle className="text-lg">Email - Mailchimp</CardTitle>
                    <p className="text-sm text-muted-foreground mt-2">
                      Connect your Mailchimp account for email campaigns
                    </p>
                    <Button variant="outline" className="mt-4" disabled>
                      Configure
                    </Button>
                  </Card>
                  
                  <Card className="p-4">
                    <CardTitle className="text-lg">Other Email Services</CardTitle>
                    <p className="text-sm text-muted-foreground mt-2">
                      Connect other email services like SendGrid, Amazon SES, etc.
                    </p>
                    <Button variant="outline" className="mt-4" disabled>
                      Configure
                    </Button>
                  </Card>
                </div>
              </CardContent>
            </Card>
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default ProjectDetail
