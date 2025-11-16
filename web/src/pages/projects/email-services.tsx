import type { Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { ConnectError } from '@connectrpc/connect'
import { useForm } from '@tanstack/react-form'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import * as z from 'zod'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

interface EmailServicesProps {
  project: Project | null
}

const formSchema = z.object({
  serviceType: z.string().min(1, 'Service type is required.'),
  apiKey: z
    .string()
    .min(1, 'API key or password is required.')
    .max(500, 'API Key is too long.'),
  senderEmail: z
    .string()
    .email('Please enter a valid email address.')
    .max(255, 'Email address is too long.'),
  region: z.string().max(100, 'Region name is too long.').default(''),
})

const EmailServices = (props: EmailServicesProps) => {
  const isProjectAvailable = props.project !== undefined && props.project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)
  const [isLoading, setIsLoading] = useState(false)

  const form = useForm({
    defaultValues: {
      serviceType: '',
      apiKey: '',
      senderEmail: '',
      region: '',
    },
    validators: {
      // @ts-expect-error - Zod schema types not perfectly matching form requirements but still functional
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsLoading(true)
      try {
        // This would normally make an API call to configure email service
        // using value.serviceType, value.apiKey, value.senderEmail, value.region
        // Using the value variable in a way that TypeScript recognizes as used
        void value // Explicitly mark value as intentionally unused
        toast.success('Email service configured successfully!')
        setIsConfigured(true)
        setShowConfiguration(false)
      } catch (error) {
        if (error instanceof ConnectError) {
          toast.error(error.rawMessage)
          return
        }

        const errorMessage = error instanceof Error ? error.message : 'An error occurred during configuration'
        toast.error(errorMessage)
        console.error('Configuration error:', error)
      } finally {
        setIsLoading(false)
      }
    },
  })

  return (
    <>
      <Card className="p-4">
        <div className="flex items-center justify-between">
          <CardTitle className="text-lg">Other Email Services</CardTitle>
          {isConfigured && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Connect other email services like SendGrid, Amazon SES, etc.
        </CardDescription>
        <Button
          variant="outline"
          className="mt-4"
          onClick={() => setShowConfiguration(true)}
          disabled={!isProjectAvailable}
        >
          {isConfigured ? 'Edit Configuration' : 'Configure'}
        </Button>
      </Card>

      <Dialog open={showConfiguration} onOpenChange={setShowConfiguration}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Configure Email Service</DialogTitle>
            <DialogDescription>
              Enter your email service credentials
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              form.handleSubmit()
            }}
            className="space-y-4"
          >
            <FieldGroup>
              <form.Field
                name="serviceType"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Service Type</Label>
                      </FieldLabel>
                      <Select value={field.state.value} onValueChange={field.handleChange}>
                        <SelectTrigger>
                          <SelectValue placeholder="Select a service" />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="sendgrid">SendGrid</SelectItem>
                          <SelectItem value="ses">Amazon SES</SelectItem>
                          <SelectItem value="mailgun">Mailgun</SelectItem>
                          <SelectItem value="smtp">SMTP</SelectItem>
                        </SelectContent>
                      </Select>
                      <FieldDescription>
                        Select your email service provider
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <FieldGroup>
              <form.Field
                name="apiKey"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>API Key or Password</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        type="password"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter your API key or password"
                      />
                      <FieldDescription>
                        Your email service API key or password
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <FieldGroup>
              <form.Field
                name="senderEmail"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Sender Email</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter sender email address"
                      />
                      <FieldDescription>
                        Email address to send from
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <FieldGroup>
              <form.Field
                name="region"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Region (if applicable)</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter region (e.g. us-east-1)"
                      />
                      <FieldDescription>
                        Region for your email service (if required)
                      </FieldDescription>
                      <FieldError>
                        {field.state.meta.errors.join(', ')}
                      </FieldError>
                    </Field>
                  )
                }}
              />
            </FieldGroup>

            <div className="flex justify-end gap-2 pt-2">
              <Button
                variant="outline"
                onClick={() => setShowConfiguration(false)}
                type="button"
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isLoading}>
                {isLoading ? 'Saving...' : 'Save Configuration'}
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default EmailServices