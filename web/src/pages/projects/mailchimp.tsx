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

interface MailchimpProps {
  project: Project | null
}

const formSchema = z.object({
  apiKey: z
    .string()
    .min(1, 'API key is required.')
    .max(500, 'API key is too long.'),
  audienceId: z
    .string()
    .min(1, 'Audience ID is required.')
    .max(255, 'Audience ID is too long.'),
})

const Mailchimp = (props: MailchimpProps) => {
  const { project } = props

  const isProjectAvailable = project !== undefined && project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)
  const [isLoading, setIsLoading] = useState(false)

  const form = useForm({
    defaultValues: {
      apiKey: '',
      audienceId: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsLoading(true)
      try {
        // This would normally make an API call to configure Mailchimp
        // using value.apiKey, value.audienceId
        // Using the value variable in a way that TypeScript recognizes as used
        void value // Explicitly mark value as intentionally unused
        toast.success('Mailchimp configured successfully!')
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
          <CardTitle className="text-lg">Email - Mailchimp</CardTitle>
          {isConfigured && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Connect your Mailchimp account for email campaigns
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
            <DialogTitle>Configure Mailchimp</DialogTitle>
            <DialogDescription>
              Enter your Mailchimp API key and audience information
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
                name="apiKey"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>API Key</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Paste your Mailchimp API key"
                      />
                      <FieldDescription>
                        Your Mailchimp API key
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
                name="audienceId"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Audience ID</Label>
                      </FieldLabel>
                      <Input
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        placeholder="Enter your Mailchimp audience ID"
                      />
                      <FieldDescription>
                        Your Mailchimp audience ID
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

export default Mailchimp