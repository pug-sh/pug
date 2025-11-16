import type { Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'

interface EmailServicesProps {
  project: Project | null
}

const EmailServices = (props: EmailServicesProps) => {
  const isProjectAvailable = props.project !== undefined && props.project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)

  const handleConfigure = () => {
    // This would normally make an API call to configure email service
    toast.success('Email service configured successfully!')
    setIsConfigured(true)
    setShowConfiguration(false)
  }

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
          <div className="space-y-4">
            <div className="space-y-2">
              <label htmlFor="serviceType" className="text-sm font-medium">Service Type</label>
              <select 
                id="serviceType" 
                className="w-full p-2 border rounded" 
              >
                <option value="">Select a service</option>
                <option value="sendgrid">SendGrid</option>
                <option value="ses">Amazon SES</option>
                <option value="mailgun">Mailgun</option>
                <option value="smtp">SMTP</option>
              </select>
            </div>
            <div className="space-y-2">
              <label htmlFor="apiKey" className="text-sm font-medium">API Key or Password</label>
              <input 
                id="apiKey" 
                type="password"
                className="w-full p-2 border rounded" 
                placeholder="Enter your API key or password" 
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="senderEmail" className="text-sm font-medium">Sender Email</label>
              <input 
                id="senderEmail" 
                className="w-full p-2 border rounded" 
                placeholder="Enter sender email address" 
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="region" className="text-sm font-medium">Region (if applicable)</label>
              <input 
                id="region" 
                className="w-full p-2 border rounded" 
                placeholder="Enter region (e.g. us-east-1)" 
              />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <Button
                variant="outline"
                onClick={() => setShowConfiguration(false)}
              >
                Cancel
              </Button>
              <Button onClick={handleConfigure}>
                Save Configuration
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}

export default EmailServices