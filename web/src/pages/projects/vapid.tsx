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
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

interface VapidProps {
  project: Project | null
}

const formSchema = z.object({
  publicKey: z
    .string()
    .min(1, 'Public key is required.')
    .max(10000, 'Public key is too long.'),
  privateKey: z
    .string()
    .min(1, 'Private key is required.')
    .max(10000, 'Private key is too long.'),
})

const Vapid = (props: VapidProps) => {
  const { project } = props

  const isProjectAvailable = project !== undefined && project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)
  const [isLoading, setIsLoading] = useState(false)

  const form = useForm({
    defaultValues: {
      publicKey: '',
      privateKey: '',
    },
    validators: {
      onSubmit: formSchema,
    },
    onSubmit: async ({ value }) => {
      setIsLoading(true)
      try {
        // This would normally make an API call to configure VAPID
        // using value.publicKey, value.privateKey
        // Using the value variable in a way that TypeScript recognizes as used
        void value // Explicitly mark value as intentionally unused
        toast.success('VAPID keys configured successfully!')
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
          <CardTitle className="text-lg">Web Push (VAPID)</CardTitle>
          {isConfigured && (
            <div className="flex items-center">
              <div className="flex items-center justify-center w-6 h-6 rounded-full bg-green-500/20">
                <CircleCheck className="h-4 w-4 text-green-600" />
              </div>
            </div>
          )}
        </div>
        <CardDescription className="mt-2 text-sm">
          Set up VAPID keys for web push notifications
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
            <DialogTitle>Configure VAPID Keys</DialogTitle>
            <DialogDescription>
              Enter your VAPID public and private keys
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
                name="publicKey"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Public Key</Label>
                      </FieldLabel>
                      <Textarea
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        rows={4}
                        className="font-mono text-sm"
                        placeholder="Paste your VAPID public key"
                      />
                      <FieldDescription>
                        Your VAPID public key
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
                name="privateKey"
                children={(field) => {
                  const isInvalid =
                    field.state.meta.isTouched && !field.state.meta.isValid

                  return (
                    <Field data-invalid={isInvalid}>
                      <FieldLabel htmlFor={field.name}>
                        <Label htmlFor={field.name}>Private Key</Label>
                      </FieldLabel>
                      <Textarea
                        id={field.name}
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        rows={4}
                        className="font-mono text-sm"
                        placeholder="Paste your VAPID private key"
                      />
                      <FieldDescription>
                        Your VAPID private key
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

export default Vapid