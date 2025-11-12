import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { selectedProjectAtom } from '@/atoms/projects'
import { useAtom } from 'jotai'
import { campaignsService } from '@/lib/rpc'

export default function NewCampaignPage() {
  const [currentProject] = useAtom(selectedProjectAtom)
  const [formData, setFormData] = useState({
    name: '',
    notificationData: '',
    scheduledTime: new Date().toISOString(),
  })
  const [isSubmitting, setIsSubmitting] = useState(false)

  const handleChange = (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    const { name, value } = e.target
    setFormData(prev => ({
      ...prev,
      [name]: value
    }))
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setIsSubmitting(true)

    try {
      const request = new CreateRequest({
        name: formData.name,
        notificationData: new TextEncoder().encode(formData.notificationData),
        projectId: currentProject.id,
        scheduledTime: {
          seconds: Math.floor(new Date(formData.scheduledTime).getTime() / 1000),
          nanos: 0,
        },
      })

      await campaignsService.create(request)
    } catch (error) {
      console.error('Error creating campaign:', error)
    } finally {
      setIsSubmitting(false)
    }
  }

  return (
    <div className="container mx-auto py-6">
      <div className="max-w-2xl mx-auto">
        <Card>
          <CardHeader>
            <CardTitle>Create New Campaign</CardTitle>
            <CardDescription>
              Create a new campaign to reach your users
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleSubmit} className="space-y-6">
              <div className="space-y-2">
                <Label htmlFor="name">Campaign Name</Label>
                <Input
                  id="name"
                  name="name"
                  value={formData.name}
                  onChange={handleChange}
                  placeholder="Enter campaign name"
                  required
                />
                <p className="text-sm text-muted-foreground">
                  A descriptive name for your campaign
                </p>
              </div>

              <div className="space-y-2">
                <Label htmlFor="notificationData">Notification Data (JSON)</Label>
                <Textarea
                  id="notificationData"
                  name="notificationData"
                  value={formData.notificationData}
                  onChange={handleChange}
                  placeholder='{ "title": "Welcome", "body": "Welcome to our platform!" }'
                  rows={6}
                  required
                />
                <p className="text-sm text-muted-foreground">
                  JSON configuration for the notification content
                </p>
              </div>

              <div className="space-y-2">
                <Label htmlFor="scheduledTime">Scheduled Time</Label>
                <Input
                  id="scheduledTime"
                  name="scheduledTime"
                  type="datetime-local"
                  value={formData.scheduledTime.slice(0, 16)} // Format to datetime-local format
                  onChange={handleChange}
                  required
                />
                <p className="text-sm text-muted-foreground">
                  When the campaign should be sent
                </p>
              </div>

              <div className="flex justify-end space-x-4">
                <Button
                  type="button"
                  variant="outline"
                >
                  Cancel
                </Button>
                <Button
                  type="submit"
                  disabled={isSubmitting}
                >
                  {isSubmitting ? 'Creating...' : 'Create Campaign'}
                </Button>
              </div>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}