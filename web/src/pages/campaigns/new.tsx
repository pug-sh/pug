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
import { DateTimePicker } from '@/components/ui/date-time-picker'
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
  scheduledTime: z.string().min(1, 'Scheduled time is required'),
})

function NewCampaign() {
  const [selectedProjectId] = useAtom(selectedProjectAtom)

  const [isSubmitting, setIsSubmitting] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)

  const [titleValue, setTitleValue] = useState('')
  const [bodyValue, setBodyValue] = useState('')
  const [scheduledDate, setScheduledDate] = useState<Date | undefined>(undefined)
  const [scheduledTime, setScheduledTime] = useState('10:00') // Default to 10:00 AM

  const form = useForm({
    defaultValues: {
      name: '',
      title: '',
      body: '',
      scheduledTime: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsSubmitting(true)
      setFormError(null)

      if (!value.scheduledTime) {
        setFormError('Please select a date for the scheduled time.')
        setIsSubmitting(false)
        return
      }
      

      try {
        const notificationObject: { title: string; body: string } = {
          title: value.title,
          body: value.body,
        }

        const combinedDateTime = new Date(value.scheduledTime);
        
        const scheduledTimeProto = { 
          seconds: BigInt(Math.floor(combinedDateTime.getTime() / 1000)), 
          nanos: 0 
        };

        const request = create(CreateRequestSchema, {
          name: value.name,
          notificationData: new TextEncoder().encode(
            JSON.stringify(notificationObject)
          ),
          projectId: selectedProjectId,
          scheduledTime: scheduledTimeProto,
        })

        await campaignsService.create(request)
        console.log('Campaign created successfully!')

        form.reset()
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
          <div className="grid grid-cols-1 lg:grid-cols-24 gap-4">
            <div className="lg:col-span-11 lg:col-start-3 space-y-2"> {/* Form takes 11 columns starting from column 3, leaving 2 columns gap on left */}
              {/* Campaign Details Card */}
              <Card>
                <CardHeader>
                  <CardTitle>Campaign Details</CardTitle>
                  <CardDescription>
                    Basic information about your campaign
                  </CardDescription>
                </CardHeader>
                <CardContent>
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
                </CardContent>
              </Card>

              {/* Notification Content Card */}
              <Card>
                <CardHeader>
                  <CardTitle>Notification Content</CardTitle>
                  <CardDescription>
                    Title and body of your notification
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <FieldGroup className="space-y-4">
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
                  </FieldGroup>
                </CardContent>
              </Card>

              {/* Scheduled Time and Submit Card */}
              <Card>
                <CardHeader>
                  <CardTitle>Scheduled Time & Submit</CardTitle>
                  <CardDescription>
                    Set when to send and create your campaign
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <div className="space-y-6">
                    <DateTimePicker
                    date={scheduledDate}
                    setDate={(date) => {
                      setScheduledDate(date);
                      // Update form field with combined datetime when date changes
                      if (date && scheduledTime) {
                        const [hours, minutes] = scheduledTime.split(':').map(Number);
                        const combinedDateTime = new Date(date);
                        combinedDateTime.setHours(hours, minutes, 0, 0);
                        form.setFieldValue('scheduledTime', combinedDateTime.toISOString());
                      } else {
                        form.setFieldValue('scheduledTime', '');
                      }
                    }}
                    time={scheduledTime}
                    setTime={(time) => {
                      setScheduledTime(time);
                      if (scheduledDate && time) {
                        const [hours, minutes] = time.split(':').map(Number);
                        const combinedDateTime = new Date(scheduledDate);
                        combinedDateTime.setHours(hours, minutes, 0, 0);
                        form.setFieldValue('scheduledTime', combinedDateTime.toISOString());
                      } else {
                        form.setFieldValue('scheduledTime', '');
                      }
                    }}
                    dateLabel="Date"
                    timeLabel="Time"
                  />

                    {formError && (
                      <div className="mb-4 text-sm text-destructive font-normal">
                        {formError}
                      </div>
                    )}
                  </div>
                </CardContent>
              </Card>

              <div className="pt-4 flex justify-end">
                <Button
                  type="submit"
                  onClick={(e) => {
                    e.preventDefault()
                    e.stopPropagation()
                    form.handleSubmit()
                  }}
                  disabled={isSubmitting}
                >
                  {isSubmitting ? 'Creating...' : 'Create Campaign'}
                </Button>
              </div>
            </div>
            
            {/* Mobile Preview Card - Right side */}
            <div className="lg:col-span-8 lg:col-start-15"> {/* Takes 8 columns and starts from column 15 */}
              <div className="sticky top-4">
                <Card>
                  <CardHeader>
                    <CardTitle>Preview</CardTitle>
                    <CardDescription>
                      How your notification will appear
                    </CardDescription>
                  </CardHeader>
                  <CardContent className="flex justify-center p-2">
                    <MobilePreview
                      notifications={previewNotifications}
                    />
                  </CardContent>
                </Card>
              </div>
            </div>
          </div>
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default NewCampaign
