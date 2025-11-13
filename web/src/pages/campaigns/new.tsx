import { CreateRequestSchema } from '@buf/pushpa_cotton.bufbuild_es/campaigns/v1/campaigns_pb'
import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { useForm } from '@tanstack/react-form'
import { useAtom } from 'jotai'
import { useMemo, useState } from 'react'
import { z } from 'zod'
import { selectedProjectAtom } from '@/atoms/projects'
import MobilePreview from '@/components/mobile-preview'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'
import { Textarea } from '@/components/ui/textarea'
import { campaignsService } from '@/lib/rpc'

const formSchema = z.object({
  name: z
    .string()
    .min(1, 'Campaign name is required')
    .max(150, 'Campaign name must be less than 150 characters'),
  title: z
    .string()
    .min(1, 'Title is required')
    .max(100, 'Title must be less than 100 characters'),
  body: z
    .string()
    .min(1, 'Body is required')
    .max(500, 'Body must be less than 500 characters'),
})

export default function NewCampaign() {
  const [selectedProjectId] = useAtom(selectedProjectAtom)

  const [isSubmitting, setIsSubmitting] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)

  const [titleValue, setTitleValue] = useState('')
  const [bodyValue, setBodyValue] = useState('')

  const form = useForm({
    defaultValues: {
      name: '',
      title: '',
      body: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsSubmitting(true)
      setFormError(null)

      try {
        const notificationObject: { title: string; body: string } = {
          title: value.title,
          body: value.body,
        }

        const request = create(CreateRequestSchema, {
          name: value.name,
          notificationData: new TextEncoder().encode(
            JSON.stringify(notificationObject)
          ),
          projectId: selectedProjectId,
        })

        await campaignsService.create(request)
        console.log('Campaign created successfully!')

        form.reset()
        setTitleValue('')
        setBodyValue('')
      } catch (error) {
        if (error instanceof ConnectError) {
          setFormError(error.rawMessage)
          console.error(error.rawMessage)
          return
        }

        const errorMessage = error instanceof Error
          ? error.message
          : 'An error occurred creating campaign'
        setFormError(errorMessage)
        console.error('Failed to create campaign')
        console.error('Error creating campaign:', error)
      } finally {
        setIsSubmitting(false)
      }
    },
  })

  const previewNotifications = useMemo(() => [
    {
      id: 1,
      appName: 'MyApp',
      appIcon: 'M',
      iconBg: '#4285f4',
      title: titleValue || 'Notification Title',
      text: bodyValue || 'Notification body will appear here',
      time: 'now',
      actions: ['Reply', 'Mark Read'],
      type: 'standard' as const,
    }
  ], [titleValue, bodyValue])

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1">
            <h1 className="text-xl font-semibold">Campaigns</h1>
          </div>
        </header>
        <div className="container mx-auto py-6">
          <div className="flex flex-col items-center lg:flex-row lg:items-start lg:justify-center lg:space-x-6">
            <div className="w-full lg:w-1/2">
              <Card>
                <CardHeader>
                  <CardTitle>Create New Campaign</CardTitle>
                  <CardDescription>
                    Create a new campaign to reach your users
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <form
                    onSubmit={(e) => {
                      e.preventDefault()
                      e.stopPropagation()
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
                              <FieldLabel htmlFor={field.name}>Campaign Name</FieldLabel>
                              <Input
                                id={field.name}
                                name={field.name}
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) => field.handleChange(e.target.value)}
                                aria-invalid={isInvalid}
                                placeholder="Enter campaign name"
                              />
                              {isInvalid && (
                                <FieldError errors={field.state.meta.errors} />
                              )}
                              <FieldDescription>
                                A descriptive name for your campaign
                              </FieldDescription>
                            </Field>
                          )
                        }}
                      />

                      <form.Field
                        name="title"
                        children={(field) => {
                          const isInvalid =
                            field.state.meta.isTouched && !field.state.meta.isValid

                          return (
                            <Field data-invalid={isInvalid}>
                              <FieldLabel htmlFor={field.name}>Title</FieldLabel>
                              <Input
                                id={field.name}
                                name={field.name}
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) => {
                                  field.handleChange(e.target.value)
                                  setTitleValue(e.target.value)
                                }}
                                aria-invalid={isInvalid}
                                placeholder="Notification title"
                              />
                              {isInvalid && (
                                <FieldError errors={field.state.meta.errors} />
                              )}
                              <FieldDescription>
                                The title of the notification
                              </FieldDescription>
                            </Field>
                          )
                        }}
                      />

                      <form.Field
                        name="body"
                        children={(field) => {
                          const isInvalid =
                            field.state.meta.isTouched && !field.state.meta.isValid

                          return (
                            <Field data-invalid={isInvalid}>
                              <FieldLabel htmlFor={field.name}>Body</FieldLabel>
                              <Textarea
                                id={field.name}
                                name={field.name}
                                value={field.state.value}
                                onBlur={field.handleBlur}
                                onChange={(e) => {
                                  field.handleChange(e.target.value)
                                  setBodyValue(e.target.value)
                                }}
                                aria-invalid={isInvalid}
                                placeholder="Enter notification body"
                                rows={3}
                              />
                              {isInvalid && (
                                <FieldError errors={field.state.meta.errors} />
                              )}
                              <FieldDescription>
                                The main body content
                              </FieldDescription>
                            </Field>
                          )
                        }}
                      />

                      <FieldGroup className="flex justify-end space-x-4 pt-4">
                        <Button
                          type="submit"
                          disabled={isSubmitting}
                        >
                          {isSubmitting ? 'Creating...' : 'Create Campaign'}
                        </Button>
                      </FieldGroup>
                    </FieldGroup>
                  </form>
                </CardContent>
              </Card>
            </div>

            <div className="mt-6 lg:mt-0">
              <MobilePreview
                notifications={previewNotifications}
              />
            </div>
          </div>
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}
