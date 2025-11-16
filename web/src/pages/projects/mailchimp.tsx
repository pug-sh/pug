import type { Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { CircleCheck } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'

interface MailchimpProps {
  project: Project | null
}

const Mailchimp = (props: MailchimpProps) => {
  const { project } = props

  const isProjectAvailable = project !== undefined && project !== null

  const [showConfiguration, setShowConfiguration] = useState(false)
  const [isConfigured, setIsConfigured] = useState(false)

  const handleConfigure = () => {
    // This would normally make an API call to configure Mailchimp
    toast.success('Mailchimp configured successfully!')
    setIsConfigured(true)
    setShowConfiguration(false)
  }

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
          <div className="space-y-4">
            <div className="space-y-2">
              <label htmlFor="apiKey" className="text-sm font-medium">API Key</label>
              <input 
                id="apiKey" 
                className="w-full p-2 border rounded" 
                placeholder="Paste your Mailchimp API key" 
              />
            </div>
            <div className="space-y-2">
              <label htmlFor="audienceId" className="text-sm font-medium">Audience ID</label>
              <input 
                id="audienceId" 
                className="w-full p-2 border rounded" 
                placeholder="Enter your Mailchimp audience ID" 
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

export default Mailchimp