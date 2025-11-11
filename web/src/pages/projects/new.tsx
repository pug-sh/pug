import { CreateRequestSchema, type CreateRequest } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { useForm } from '@tanstack/react-form'
import { useState } from 'react'
import * as z from 'zod'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Field,
  FieldError,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'
import { Spinner } from '@/components/ui/spinner'
import { projectsService } from '@/lib/rpc'

const formSchema = z.object({
  name: z
    .string()
    .min(2, 'Project name must be at least 2 characters.')
    .max(100, 'Project name must not exceed 100 characters.'),
})

function NewProject() {
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)

  const form = useForm({
    defaultValues: {
      name: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      console.log('Form submitted with value:', value)
      setIsSubmitting(true)
      setFormError(null)
      try {
        const request: CreateRequest = create(CreateRequestSchema, {
          displayName: value.name
        })
        
        console.log('Sending request:', request)
        await projectsService.create(request)
        
        // Show success message
        alert(`Project "${request.displayName}" created successfully!`)
      } catch (error) {
        if (error instanceof ConnectError) {
          setFormError(error.rawMessage)
          return
        }

        const errorMessage = error instanceof Error ? error.message : 'An error occurred creating project'
        setFormError(errorMessage)
        console.error('Create project error:', error)
      } finally {
        setIsSubmitting(false)
      }
    },
  })

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Create New Project</h1>
          </div>
        </header>
        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-2xl mx-auto">
            <Card>
              <CardHeader>
                <CardTitle>Create Project</CardTitle>
                <CardDescription>Enter the details for your new project</CardDescription>
              </CardHeader>
              <CardContent>
                <form
                  onSubmit={(e) => {
                    e.preventDefault()
                    form.handleSubmit()
                  }}
                  className="space-y-6"
                >
                  {formError && (
                    <div className="mb-4 text-sm text-destructive font-normal">
                      {formError}
                    </div>
                  )}
                  <FieldGroup>
                    <form.Field
                      name="name"
                      children={(field) => {
                        const isInvalid =
                          field.state.meta.isTouched && !field.state.meta.isValid

                        return (
                          <Field data-invalid={isInvalid}>
                            <FieldLabel htmlFor={field.name}>
                              <Label htmlFor={field.name}>Project Name</Label>
                            </FieldLabel>
                            <Input
                              id={field.name}
                              name={field.name}
                              value={field.state.value}
                              onBlur={field.handleBlur}
                              onChange={(e) => field.handleChange(e.target.value)}
                              aria-invalid={isInvalid}
                              placeholder="Enter project name"
                              type="text"
                              autoComplete="off"
                            />
                            {isInvalid && (
                              <FieldError errors={field.state.meta.errors} />
                            )}
                          </Field>
                        )
                      }}
                    />
                  </FieldGroup>

                  <div className="flex justify-end space-x-3">
                    <Button
                      type="button"
                      variant="outline"
                      onClick={() => window.history.back()}
                    >
                      Cancel
                    </Button>
                    <Button
                      type="submit"
                      disabled={isSubmitting}
                    >
                      {isSubmitting ? (
                        <>
                          <Spinner className="mr-2 h-4 w-4" />
                          Creating...
                        </>
                      ) : (
                        'Create Project'
                      )}
                    </Button>
                  </div>
                </form>
              </CardContent>
            </Card>
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default NewProject
