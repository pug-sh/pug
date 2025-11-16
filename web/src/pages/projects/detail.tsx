import {
  type Project,
  UpdateDisplayNameRequestSchema
} from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { useForm } from '@tanstack/react-form'
import { Copy } from 'lucide-react'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { useParams } from 'wouter'
import * as z from 'zod'
import ApplePushNotifications from './apple-push-notifications'
import EmailServices from './email-services'
import FirebaseIntegration from './firebase'
import Mailchimp from './mailchimp'
import Vapid from './vapid'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Field,
  FieldError,
  FieldGroup,
} from '@/components/ui/field'
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
  const [updating, setUpdating] = useState(false)

  const formSchema = z.object({
    displayName: z
      .string()
      .min(2, 'Project name must be at least 2 characters.')
      .max(150, 'Project name must not exceed 150 characters.'),
  })

  const form = useForm({
    defaultValues: {
      displayName: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      if (!project) return

      try {
        setUpdating(true)
        const request = create(UpdateDisplayNameRequestSchema, {
          displayName: value.displayName,
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
    },
  })


  useEffect(() => {
    const fetchProject = async () => {
      if (!id) return

      try {
        setLoading(true)
        const response = await projectsService.get({ id })
        if (response.project) {
          setProject(response.project)
          if (!editingName) {
            form.reset()
            form.setFieldValue('displayName', response.project.displayName)
          }
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
  }, [id, editingName, form])

  const handleCopyApiKey = () => {
    if (project) {
      navigator.clipboard.writeText(project.apiKey)
      toast.success('API Key copied to clipboard!')
    }
  }

  const handleCopyId = () => {
    if (project) {
      navigator.clipboard.writeText(project.id)
      toast.success('Project ID copied to clipboard!')
    }
  }


  if (loading) {
    return (
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <header className="sticky top-0 z-10 flex h-16 items-center gap-4
                             border-b bg-background px-4 sm:px-6 lg:px-8">
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
          <header className="sticky top-0 z-10 flex h-16 items-center gap-4
                             border-b bg-background px-4 sm:px-6 lg:px-8">
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
        <header className="sticky top-0 z-10 flex h-16 items-center gap-4
                           border-b bg-background px-4 sm:px-6 lg:px-8">
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
                  <form
                    onSubmit={(e) => {
                      e.preventDefault()
                      form.handleSubmit()
                    }}
                    className="flex flex-col sm:flex-row gap-2 items-start sm:items-center"
                  >
                    <FieldGroup>
                      <form.Field
                        name="displayName"
                        children={(field) => {
                          const isInvalid =
                            field.state.meta.isTouched && !field.state.meta.isValid

                          return (
                            <Field data-invalid={isInvalid}>
                              <Input
                                id={field.name}
                                name={field.name}
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) => field.handleChange(e.target.value)}
                                className="flex-1"
                                placeholder="Project name"
                                aria-invalid={isInvalid}
                              />
                              {isInvalid && (
                                <FieldError errors={field.state.meta.errors} />
                              )}
                            </Field>
                          )
                        }}
                      />
                    </FieldGroup>
                    <div className="flex gap-2">
                      <Button type="submit" disabled={updating}>
                        {updating ? 'Saving...' : 'Save'}
                      </Button>
                      <Button
                        variant="outline"
                        onClick={() => {
                          setEditingName(false)
                          form.reset()
                        }}
                        disabled={updating}
                      >
                        Cancel
                      </Button>
                    </div>
                  </form>
                ) : (
                  <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4">
                    <h2 className="text-xl font-bold">{project?.displayName}</h2>
                    <Button
                      onClick={() => {
                        if (project) {
                          form.setFieldValue('displayName', project.displayName)
                          setEditingName(true)
                        }
                      }}
                    >
                      Edit Name
                    </Button>
                  </div>
                )}

                <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                  <div>
                    <Label className="mb-2">Project ID</Label>
                    <div className="flex gap-2">
                      <Input
                        value={project?.id || ''}
                        disabled
                        className="font-mono text-xs"
                      />
                      <Button variant="outline" size="icon" onClick={() => handleCopyId()} title="Copy Project ID">
                        <Copy className="h-4 w-4" />
                      </Button>
                    </div>
                  </div>

                  <div>
                    <Label className="mb-2">API Key</Label>
                    <div className="flex gap-2">
                      <Input
                        value={project?.apiKey || ''}
                        disabled
                        className="font-mono text-xs"
                      />
                      <Button variant="outline" size="icon" onClick={handleCopyApiKey} title="Copy API Key">
                        <Copy className="h-4 w-4" />
                      </Button>
                    </div>
                  </div>
                </div>
              </CardContent>
            </Card>
            {/* Additional Integrations - Apple Push Notifications, VAPID, etc. */}
            <Card>
              <CardHeader>
                <CardTitle>Additional Integrations</CardTitle>
                <CardDescription>
                  Configure other services for your project
                </CardDescription>
              </CardHeader>
              <CardContent>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                  <FirebaseIntegration
                    project={project}
                    onProjectUpdate={(updatedProject) => setProject(updatedProject)}
                  />
                  <ApplePushNotifications project={project} />
                  <Vapid project={project} />
                  <Mailchimp project={project} />
                  <EmailServices project={project} />
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
